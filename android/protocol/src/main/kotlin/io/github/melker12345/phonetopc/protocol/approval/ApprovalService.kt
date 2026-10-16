package io.github.melker12345.phonetopc.protocol.approval

import io.github.melker12345.phonetopc.protocol.PcInfo
import io.github.melker12345.phonetopc.protocol.invalidParams
import io.github.melker12345.phonetopc.protocol.security.Base64Url
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancelChildren
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeoutOrNull
import java.security.SecureRandom
import java.util.Random
import java.util.concurrent.atomic.AtomicLong
import kotlin.coroutines.cancellation.CancellationException

/**
 * Runs `request_approval` (APPROVAL.md §4) and `decrypt` (DECRYPT.md §4) for every session of a
 * hub: one pending per PC, those of different PCs shown one after another, the timeout, the
 * numbers to pick from, the cool-down (§4.3), and a check of the signature before it leaves the
 * phone.
 *
 * @param scope where approvals run; sessions cancel their own approval when they close.
 * @param clock the phone's clock in Unix milliseconds, for `expiry` and the cool-down.
 */
class ApprovalService(
    private val approver: Approver,
    private val scope: CoroutineScope = CoroutineScope(SupervisorJob() + Dispatchers.Default),
    private val onError: (Throwable) -> Unit = {},
    val clock: () -> Long = System::currentTimeMillis,
    private val random: Random = SecureRandom(),
) {
    private class Slot(val pcId: String) {
        val turn = CompletableDeferred<Unit>()
    }

    private class Cooldown {
        val refusals = ArrayDeque<Long>()
        var until = Long.MIN_VALUE
    }

    private val lock = Any()
    private var shown: Slot? = null
    private val waiting = ArrayDeque<Slot>()
    private val cooldowns = HashMap<String, Cooldown>()
    private val nextId = AtomicLong(1)

    /** `hello.approval`, or `null` when the capability isn't offered. Never throws. */
    fun keyInfo(): ApprovalKeyInfo? = try {
        approver.keyInfo()
    } catch (e: Exception) {
        onError(e)
        null
    }

    /** `hello.decrypt`, or `null` when the capability isn't offered. Never throws. */
    fun decryptKeyInfo(): ApprovalKeyInfo? = try {
        approver.decryptKeyInfo()
    } catch (e: Exception) {
        onError(e)
        null
    }

    sealed interface Start {
        /** Answered at once (`unavailable`, `cooldown`, `busy`); `respond` is not called. */
        data class Answered(val outcome: ApprovalOutcome) : Start

        /** [startDecrypt] answered at once. */
        data class DecryptAnswered(val outcome: DecryptOutcome) : Start

        /** Running; `respond` is called exactly once unless [job] is cancelled first. */
        class Pending(val job: Job) : Start
    }

    /**
     * @param announced the key this session's `hello` reported. The PC verifies with that one,
     *   so if there was none, or the key has been replaced since, the outcome is `unavailable`.
     * @param respond gets the outcome of a [Start.Pending] approval, from the service's scope.
     */
    fun start(
        phoneId: String,
        pc: PcInfo,
        announced: ApprovalKeyInfo?,
        params: ApprovalParams,
        respond: (ApprovalOutcome) -> Unit,
    ): Start {
        val key = keyInfo()
        if (key == null || key != announced) return Start.Answered(ApprovalOutcome.Refused(ApprovalWhy.UNAVAILABLE))
        if (inCooldown(pc.id)) return Start.Answered(ApprovalOutcome.Refused(ApprovalWhy.COOLDOWN))
        val slot = Slot(pc.id)
        if (!enter(slot)) return Start.Answered(ApprovalOutcome.Refused(ApprovalWhy.BUSY))
        val request = ApprovalRequest(
            id = nextId.getAndIncrement(),
            pc = pc,
            program = params.program,
            reason = params.reason,
            nonce = params.nonce,
            match = params.match,
            choices = choices(params.match),
            timeoutMillis = params.timeoutMillis,
            message = ApprovalMessage.build(phoneId, pc.id, params.nonce, params.reason, params.program, params.match, params.expiry),
        )
        return Start.Pending(
            run(slot, request.timeoutMillis, { approver.approve(request) }, ApprovalOutcome::Refused) {
                if (it is ApprovalOutcome.Refused) noteOutcome(pc.id, it.why)
                respond(checked(it, key, request))
            },
        )
    }

    /**
     * Like [start], for `decrypt`. [announced] is the decryption key this session's `hello`
     * reported; a ciphertext for any other key is `unavailable` without asking the user.
     *
     * @throws io.github.melker12345.phonetopc.protocol.RpcException `-32602` for a ciphertext
     *   whose length doesn't fit the key.
     */
    fun startDecrypt(
        pc: PcInfo,
        announced: ApprovalKeyInfo?,
        params: DecryptParams,
        respond: (DecryptOutcome) -> Unit,
    ): Start {
        val key = decryptKeyInfo()
        val size = key?.let(DecryptCipher::ciphertextSize)
        if (size != null && params.ciphertext.size != size) invalidParams("ciphertext must be $size bytes for this key")
        if (key == null || key != announced || key.pin != params.keyPin) {
            return Start.DecryptAnswered(DecryptOutcome.Refused(ApprovalWhy.UNAVAILABLE))
        }
        if (inCooldown(pc.id)) return Start.DecryptAnswered(DecryptOutcome.Refused(ApprovalWhy.COOLDOWN))
        val slot = Slot(pc.id)
        if (!enter(slot)) return Start.DecryptAnswered(DecryptOutcome.Refused(ApprovalWhy.BUSY))
        val request = DecryptRequest(
            nextId.getAndIncrement(), pc, params.program, params.reason,
            params.match, choices(params.match), params.timeoutMillis, params.ciphertext,
        )
        return Start.Pending(
            run(slot, request.timeoutMillis, { approver.decrypt(request) }, DecryptOutcome::Refused) {
                if (it is DecryptOutcome.Refused) noteOutcome(pc.id, it.why)
                respond(it)
            },
        )
    }

    /** [match] and two other numbers, all different, in random order (APPROVAL.md §4.2). */
    private fun choices(match: Int): List<Int> {
        val numbers = mutableListOf(match)
        while (numbers.size < 3) {
            val n = ApprovalParams.MIN_MATCH + random.nextInt(ApprovalParams.MAX_MATCH - ApprovalParams.MIN_MATCH + 1)
            if (n !in numbers) numbers += n
        }
        return numbers.shuffled(random)
    }

    private fun inCooldown(pcId: String): Boolean = synchronized(lock) {
        val c = cooldowns[pcId] ?: return false
        clock() < c.until
    }

    /** Counts a refusal towards [pcId]'s cool-down (APPROVAL.md §4.3). */
    private fun noteOutcome(pcId: String, why: ApprovalWhy) {
        if (!why.refusal) return
        synchronized(lock) {
            val now = clock()
            val c = cooldowns.getOrPut(pcId, ::Cooldown)
            while (c.refusals.isNotEmpty() && now - c.refusals.first() >= COOLDOWN_WINDOW_MS) c.refusals.removeFirst()
            c.refusals.addLast(now)
            if (c.refusals.size >= COOLDOWN_REFUSALS) {
                c.refusals.clear()
                c.until = now + COOLDOWN_MS
            }
        }
    }

    /** Runs [ask] once it is [slot]'s turn; [enter] gave the caller the slot. */
    private fun <O> run(
        slot: Slot,
        timeoutMillis: Long,
        ask: suspend () -> O,
        refused: (ApprovalWhy) -> O,
        respond: (O) -> Unit,
    ): Job {
        val job = scope.launch {
            val outcome = try {
                withTimeoutOrNull(timeoutMillis) {
                    slot.turn.await()
                    ask()
                } ?: refused(ApprovalWhy.TIMEOUT)
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                onError(e)
                refused(ApprovalWhy.UNAVAILABLE)
            } finally {
                // Free before answering, so the PC may ask again as soon as it has the answer.
                leave(slot)
            }
            respond(outcome)
        }
        // A job cancelled before it ever ran skips its finally block.
        job.invokeOnCompletion { leave(slot) }
        return job
    }

    /** False if [slot]'s PC already has a request pending, or [MAX_WAITING] PCs are waiting. */
    private fun enter(slot: Slot): Boolean = synchronized(lock) {
        if (shown?.pcId == slot.pcId || waiting.any { it.pcId == slot.pcId }) return false
        when {
            shown == null -> {
                shown = slot
                slot.turn.complete(Unit)
            }
            waiting.size < MAX_WAITING -> waiting.addLast(slot)
            else -> return false
        }
        true
    }

    /** Idempotent. If [slot] was shown, the next waiting one gets its turn. */
    private fun leave(slot: Slot) {
        synchronized(lock) {
            if (shown !== slot) {
                waiting.remove(slot)
                return
            }
            shown = waiting.removeFirstOrNull()
            shown?.turn?.complete(Unit)
        }
    }

    /**
     * Never send a signature the PC would reject: if it doesn't verify with the key `hello`
     * announced (e.g. the key was replaced meanwhile), answer `unavailable` instead.
     */
    private fun checked(outcome: ApprovalOutcome, key: ApprovalKeyInfo, request: ApprovalRequest): ApprovalOutcome {
        if (outcome !is ApprovalOutcome.Approved) return outcome
        val spki = Base64Url.decode(key.key)
        if (spki != null && ApprovalMessage.verify(spki, request.message, outcome.signature)) return outcome
        onError(IllegalStateException("approval signature does not verify with the announced key"))
        return ApprovalOutcome.Refused(ApprovalWhy.UNAVAILABLE)
    }

    /** Ends every pending approval (server stopping). */
    fun cancelAll() = scope.coroutineContext[Job]?.cancelChildren()

    companion object {
        /** PCs whose request waits behind the one shown; more are answered `busy`. */
        const val MAX_WAITING = 8

        /** [COOLDOWN_REFUSALS] refusals within [COOLDOWN_WINDOW_MS] start a cool-down of [COOLDOWN_MS]. */
        const val COOLDOWN_REFUSALS = 3
        const val COOLDOWN_WINDOW_MS = 10 * 60_000L
        const val COOLDOWN_MS = 5 * 60_000L
    }
}
