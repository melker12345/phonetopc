package io.github.melker12345.phonetopc.protocol.approval

import io.github.melker12345.phonetopc.protocol.PcInfo
import io.github.melker12345.phonetopc.protocol.security.Base64Url
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancelChildren
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeoutOrNull
import java.util.concurrent.atomic.AtomicLong
import kotlin.coroutines.cancellation.CancellationException

/**
 * Runs `request_approval` (APPROVAL.md §4) for every session of a hub: one approval at a time
 * per phone, the timeout, and a check of the signature before it leaves the phone.
 *
 * @param scope where approvals run; sessions cancel their own approval when they close.
 */
class ApprovalService(
    private val approver: Approver,
    private val scope: CoroutineScope = CoroutineScope(SupervisorJob() + Dispatchers.Default),
    private val onError: (Throwable) -> Unit = {},
) {
    /** Id of the pending approval, 0 when none. */
    private val pending = AtomicLong(0)
    private val nextId = AtomicLong(1)

    /** `hello.approval`, or `null` when the capability isn't offered. Never throws. */
    fun keyInfo(): ApprovalKeyInfo? = try {
        approver.keyInfo()
    } catch (e: Exception) {
        onError(e)
        null
    }

    /** What [start] did. */
    sealed interface Start {
        /** Answered at once (`unavailable`, `busy`); `respond` is not called. */
        data class Answered(val outcome: ApprovalOutcome) : Start

        /** Running; `respond` is called exactly once unless [job] is cancelled first. */
        class Pending(val job: Job) : Start
    }

    /**
     * Starts an approval.
     *
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
        val id = nextId.getAndIncrement()
        if (!pending.compareAndSet(0, id)) return Start.Answered(ApprovalOutcome.Refused(ApprovalWhy.BUSY))
        val request = ApprovalRequest(
            id = id,
            pc = pc,
            reason = params.reason,
            nonce = params.nonce,
            timeoutMillis = params.timeoutMillis,
            message = ApprovalMessage.build(phoneId, pc.id, params.nonce, params.reason),
        )
        val job = scope.launch {
            val outcome = try {
                withTimeoutOrNull(request.timeoutMillis) { approver.approve(request) }
                    ?: ApprovalOutcome.Refused(ApprovalWhy.TIMEOUT)
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                onError(e)
                ApprovalOutcome.Refused(ApprovalWhy.UNAVAILABLE)
            } finally {
                // Free before answering, so the PC may ask again as soon as it has the answer.
                pending.compareAndSet(id, 0)
            }
            respond(checked(outcome, key, request))
        }
        // A job cancelled before it ever ran skips its finally block.
        job.invokeOnCompletion { pending.compareAndSet(id, 0) }
        return Start.Pending(job)
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
}
