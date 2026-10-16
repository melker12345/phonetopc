package io.github.melker12345.phonetopc.protocol.approval

import io.github.melker12345.phonetopc.protocol.BridgeHub
import io.github.melker12345.phonetopc.protocol.DeviceInfo
import io.github.melker12345.phonetopc.protocol.FakeSensorSource
import io.github.melker12345.phonetopc.protocol.PcInfo
import io.github.melker12345.phonetopc.protocol.ProtocolJson
import io.github.melker12345.phonetopc.protocol.SecureAccess
import io.github.melker12345.phonetopc.protocol.ServerInfo
import io.github.melker12345.phonetopc.protocol.Session
import io.github.melker12345.phonetopc.protocol.security.Base64Url
import io.github.melker12345.phonetopc.protocol.security.Secrets
import io.github.melker12345.phonetopc.protocol.security.TokenHash
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.test.TestScope
import kotlinx.coroutines.test.advanceTimeBy
import kotlinx.coroutines.test.runCurrent
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.boolean
import kotlinx.serialization.json.int
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import java.security.KeyPairGenerator
import java.security.Signature
import java.security.spec.ECGenParameterSpec
import kotlin.coroutines.cancellation.CancellationException

/** `request_approval` in the session (APPROVAL.md §1, §4), with a fake signer and UI. */
@OptIn(ExperimentalCoroutinesApi::class)
class ApprovalSessionTest {
    private class FakeApprover : Approver {
        var keyPair = newKeyPair()
        var available = true
        val requests = mutableListOf<ApprovalRequest>()
        var answer = CompletableDeferred<ApprovalOutcome>()
        var cancellations = 0

        override fun keyInfo() = if (available) ApprovalKeyInfo.of(keyPair.public.encoded) else null

        override suspend fun approve(request: ApprovalRequest): ApprovalOutcome {
            requests += request
            try {
                return answer.await()
            } catch (e: CancellationException) {
                cancellations++
                throw e
            }
        }

        fun sign(message: ByteArray): ByteArray = Signature.getInstance(ApprovalMessage.ALGORITHM).run {
            initSign(keyPair.private)
            update(message)
            sign()
        }

        companion object {
            fun newKeyPair() = KeyPairGenerator.getInstance("EC").apply { initialize(ECGenParameterSpec("secp256r1")) }.generateKeyPair()
        }
    }

    private val pc = PcInfo("81d7d5f3b885fd1d", "arch-desktop")
    private val otherPcs = (1..ApprovalService.MAX_WAITING + 1).map { PcInfo("pc$it", "laptop $it") }
    private val tokens = (listOf(pc) + otherPcs).associateBy { Secrets.newToken() }
    private val approver = FakeApprover()
    private val scope = TestScope()
    private val service = ApprovalService(approver, scope)

    private fun hub(approvals: ApprovalService? = service, secure: Boolean = true) = BridgeHub(
        source = FakeSensorSource(),
        serverInfo = ServerInfo("phonetopc-android", "test"),
        deviceInfoProvider = { DeviceInfo("635f3a01db1859bc", "Test Phone", platform = "android") },
        security = if (secure) SecureAccess(authenticate = { t -> tokens.entries.firstOrNull { TokenHash.matches(t, TokenHash.of(it.key)) }?.value }) else null,
        approvals = approvals,
    )

    private val hub = hub()

    private fun Session.drain(): List<JsonObject> =
        generateSequence { tryReceiveOutgoing() }.map { ProtocolJson.parseToJsonElement(it).jsonObject }.toList()

    private fun Session.send(text: String): List<JsonObject> = runBlocking {
        handleText(text)
        drain()
    }

    private fun Session.helloResult(who: PcInfo): JsonObject {
        val token = tokens.entries.single { it.value == who }.key
        return send("""{"jsonrpc":"2.0","id":0,"method":"hello","params":{"protocol":"phonetopc/1","auth":{"token":"$token"}}}""")
            .single()["result"]!!.jsonObject
    }

    private fun session(h: BridgeHub = hub, who: PcInfo = pc): Pair<Session, JsonObject> =
        h.openSession("pc").let { it to it.helloResult(who) }

    private val nonce = Base64Url.encode(Secrets.bytes(32))
    private val reason = "Run deploy.sh on arch-desktop?"

    private val program = "deploy.sh"
    private val expiry = System.currentTimeMillis() + 40_000

    private fun approvalRequest(id: Int = 7, timeout: Int? = null, n: String = nonce, match: Int = 42) =
        """{"jsonrpc":"2.0","id":$id,"method":"request_approval","params":{"reason":"$reason","program":"$program",""" +
            """"nonce":"$n","match":$match,"expiry":$expiry""" + (timeout?.let { ""","timeout_ms":$it""" } ?: "") + "}}"

    private fun JsonObject.result() = this["result"]!!.jsonObject
    private fun JsonObject.errorCode() = this["error"]?.jsonObject?.get("code")?.jsonPrimitive?.int
    private fun JsonObject.why() = result()["why"]?.jsonPrimitive?.content

    @Test fun helloAnnouncesTheKey() {
        val (_, hello) = session()
        val caps = hello["capabilities"]!!.jsonArray.map { it.jsonPrimitive.content }
        assertTrue("approval" in caps)
        val approval = hello["approval"]!!.jsonObject
        val info = ApprovalKeyInfo.of(approver.keyPair.public.encoded)
        assertEquals(info.key, approval["key"]!!.jsonPrimitive.content)
        assertEquals(info.pin, approval["pin"]!!.jsonPrimitive.content)
    }

    @Test fun noCapabilityWithoutBiometrics() {
        approver.available = false
        val (s, hello) = session()
        assertTrue("approval" !in hello["capabilities"]!!.jsonArray.map { it.jsonPrimitive.content })
        assertNull(hello["approval"])
        assertEquals("unavailable", s.send(approvalRequest()).single().why())
        assertTrue(approver.requests.isEmpty())
    }

    @Test fun secureModeWithoutAnApproverAnswersUnavailable() {
        val (s, hello) = session(hub(approvals = null))
        assertNull(hello["approval"])
        assertEquals("unavailable", s.send(approvalRequest()).single().why())
    }

    @Test fun devModeHasNoSuchMethod() {
        val dev = hub(secure = false)
        val s = dev.openSession("pc")
        // Before hello it's -32003 like any request.
        assertEquals(-32003, s.send(approvalRequest()).single().errorCode())
        val hello = s.send("""{"jsonrpc":"2.0","id":0,"method":"hello","params":{"protocol":"phonetopc/1"}}""").single().result()
        assertNull(hello["approval"])
        assertTrue("approval" !in hello["capabilities"]!!.jsonArray.map { it.jsonPrimitive.content })
        assertEquals(-32601, s.send(approvalRequest()).single().errorCode())
    }

    @Test fun invalidParamsAreErrorsNotResults() {
        val (s, _) = session()
        assertEquals(-32602, s.send(approvalRequest(n = "short")).single().errorCode())
        assertEquals(-32602, s.send(approvalRequest(timeout = 1000)).single().errorCode())
        assertEquals(-32602, s.send(approvalRequest(match = 100)).single().errorCode())
        assertEquals(-32602, s.send(approvalRequest().replace(",\"expiry\":$expiry", "")).single().errorCode())
        assertEquals(-32602, s.send(approvalRequest().replace(",\"program\":\"$program\"", "")).single().errorCode())
        assertEquals(-32602, s.send("""{"jsonrpc":"2.0","id":1,"method":"request_approval"}""").single().errorCode())
        assertEquals(-32602, s.send("""{"jsonrpc":"2.0","id":1,"method":"request_approval","params":[]}""").single().errorCode())
        // Even an unavailable phone checks params first.
        approver.available = false
        assertEquals(-32602, s.send(approvalRequest(n = "short")).single().errorCode())
    }

    @Test fun approvedSignatureVerifiesOverThePcsOwnMessage() {
        val (s, hello) = session()
        assertTrue("answered only once the user acted", s.send(approvalRequest()).isEmpty())
        scope.runCurrent()
        val request = approver.requests.single()
        assertEquals(pc, request.pc)
        assertEquals(reason, request.reason)
        assertEquals(program, request.program)
        assertEquals(42, request.match)
        assertEquals(30_000L, request.timeoutMillis)
        // §5: phone id from hello.device.id, pc id from hello.pc.id.
        val phoneId = hello["device"]!!.jsonObject["id"]!!.jsonPrimitive.content
        val pcId = hello["pc"]!!.jsonObject["id"]!!.jsonPrimitive.content
        val pcMessage = ApprovalMessage.build(phoneId, pcId, nonce, reason, program, 42, expiry)
        assertArrayEquals(pcMessage, request.message)

        approver.answer.complete(ApprovalOutcome.Approved(approver.sign(request.message)))
        scope.runCurrent()
        val r = s.drain().single()
        assertEquals(7, r["id"]!!.jsonPrimitive.int)
        assertTrue(r.result()["approved"]!!.jsonPrimitive.boolean)
        val signature = r.result()["signature"]!!.jsonPrimitive.content
        val key = hello["approval"]!!.jsonObject["key"]!!.jsonPrimitive.content
        assertTrue(ApprovalMessage.verify(key, pcMessage, signature))
    }

    @Test fun otherRequestsAreAnsweredMeanwhile() {
        val (s, _) = session()
        s.send(approvalRequest())
        scope.runCurrent()
        val r = s.send("""{"jsonrpc":"2.0","id":8,"method":"list_sensors"}""").single()
        assertEquals(8, r["id"]!!.jsonPrimitive.int)
        approver.answer.complete(ApprovalOutcome.Refused(ApprovalWhy.DENIED))
        scope.runCurrent()
        assertEquals("denied", s.drain().single().why())
    }

    @Test fun oneAtATimePerPc() {
        val (a, _) = session()
        val (b, _) = session()
        a.send(approvalRequest(id = 1))
        scope.runCurrent()
        assertEquals("busy", b.send(approvalRequest(id = 2)).single().why())
        assertEquals("busy", a.send(approvalRequest(id = 3)).single().why())
        approver.answer.complete(ApprovalOutcome.Refused(ApprovalWhy.CANCELLED))
        scope.runCurrent()
        assertEquals("cancelled", a.drain().single().why())
        // Free again once answered.
        approver.answer = CompletableDeferred()
        assertTrue(b.send(approvalRequest(id = 4)).isEmpty())
        scope.runCurrent()
        assertEquals(2, approver.requests.size)
    }

    @Test fun otherPcsWaitTheirTurn() {
        val (a, _) = session()
        val (b, _) = session(who = otherPcs[0])
        val (c, _) = session(who = otherPcs[1])
        assertTrue(a.send(approvalRequest(id = 1)).isEmpty())
        assertTrue(b.send(approvalRequest(id = 2)).isEmpty())
        assertTrue(c.send(approvalRequest(id = 3)).isEmpty())
        scope.runCurrent()
        assertEquals(listOf(pc), approver.requests.map { it.pc })
        assertEquals("busy", b.send(approvalRequest(id = 4)).single().why())

        approver.answer.complete(ApprovalOutcome.Refused(ApprovalWhy.DENIED))
        approver.answer = CompletableDeferred()
        scope.runCurrent()
        assertEquals("denied", a.drain().single().why())
        assertEquals(listOf(pc, otherPcs[0]), approver.requests.map { it.pc })

        val signed = approver.requests.last().message
        approver.answer.complete(ApprovalOutcome.Approved(approver.sign(signed)))
        scope.runCurrent()
        assertTrue(b.drain().single().result()["approved"]!!.jsonPrimitive.boolean)
        assertEquals(listOf(pc, otherPcs[0], otherPcs[1]), approver.requests.map { it.pc })
    }

    @Test fun aWaitingRequestTimesOutUnseen() {
        val (a, _) = session()
        val (b, _) = session(who = otherPcs[0])
        a.send(approvalRequest(id = 1, timeout = 10_000))
        b.send(approvalRequest(id = 2, timeout = 5_000))
        scope.runCurrent()
        scope.advanceTimeBy(5_001)
        scope.runCurrent()
        assertEquals("timeout", b.drain().single().why())
        assertTrue(a.drain().isEmpty())
        approver.answer.complete(ApprovalOutcome.Refused(ApprovalWhy.DENIED))
        scope.runCurrent()
        assertEquals("denied", a.drain().single().why())
        assertEquals(listOf(pc), approver.requests.map { it.pc })
        approver.answer = CompletableDeferred()
        assertTrue("may ask again", b.send(approvalRequest(id = 3)).isEmpty())
        scope.runCurrent()
        assertEquals(otherPcs[0], approver.requests.last().pc)
    }

    @Test fun aWaitingRequestOfAClosedSessionVanishes() {
        val (a, _) = session()
        val (b, _) = session(who = otherPcs[0])
        val (c, _) = session(who = otherPcs[1])
        a.send(approvalRequest(id = 1))
        b.send(approvalRequest(id = 2))
        c.send(approvalRequest(id = 3))
        scope.runCurrent()
        b.close()
        scope.runCurrent()
        approver.answer.complete(ApprovalOutcome.Refused(ApprovalWhy.DENIED))
        approver.answer = CompletableDeferred()
        scope.runCurrent()
        assertEquals("denied", a.drain().single().why())
        assertTrue(b.drain().isEmpty())
        assertEquals(listOf(pc, otherPcs[1]), approver.requests.map { it.pc })
        assertEquals(0, approver.cancellations)
    }

    @Test fun theQueueIsBounded() {
        val sessions = (listOf(pc) + otherPcs).map { session(who = it).first }
        val last = sessions.last()
        sessions.dropLast(1).forEachIndexed { i, s -> assertTrue(s.send(approvalRequest(id = i)).isEmpty()) }
        scope.runCurrent()
        assertEquals("busy", last.send(approvalRequest(id = 99)).single().why())
        approver.answer.complete(ApprovalOutcome.Refused(ApprovalWhy.DENIED))
        scope.runCurrent()
        assertTrue("a place is free again", last.send(approvalRequest(id = 100)).isEmpty())
    }

    @Test fun timeoutEndsThePrompt() {
        val (s, _) = session()
        s.send(approvalRequest(timeout = 5000))
        scope.runCurrent()
        scope.advanceTimeBy(4_999)
        scope.runCurrent()
        assertTrue(s.drain().isEmpty())
        scope.advanceTimeBy(2)
        scope.runCurrent()
        assertEquals("timeout", s.drain().single().why())
        assertEquals("the UI was told to go away", 1, approver.cancellations)
        // And the phone is free again.
        approver.answer = CompletableDeferred()
        assertTrue(s.send(approvalRequest(id = 9)).isEmpty())
    }

    @Test fun closingTheSessionCancelsThePrompt() {
        val (s, _) = session()
        s.send(approvalRequest())
        scope.runCurrent()
        s.close()
        scope.runCurrent()
        assertEquals(1, approver.cancellations)
        assertTrue(s.drain().isEmpty())
        val (other, _) = session()
        approver.answer = CompletableDeferred()
        assertTrue("not busy any more", other.send(approvalRequest()).isEmpty())
    }

    @Test fun closingBeforeTheApprovalRanFreesThePhone() {
        val (s, _) = session()
        s.send(approvalRequest())
        s.close() // the approval's coroutine never started
        scope.runCurrent()
        val (other, _) = session()
        assertTrue(other.send(approvalRequest()).isEmpty())
    }

    @Test fun outcomesPassThrough() {
        for (why in listOf(ApprovalWhy.DENIED, ApprovalWhy.CANCELLED, ApprovalWhy.LOCKOUT, ApprovalWhy.UNAVAILABLE, ApprovalWhy.MISMATCH)) {
            val (s, _) = session()
            approver.answer = CompletableDeferred(ApprovalOutcome.Refused(why))
            s.send(approvalRequest())
            scope.runCurrent()
            assertEquals(why.wire, s.drain().single().why())
        }
    }

    @Test fun aSignatureThatDoesntVerifyIsNeverSent() {
        val (s, _) = session()
        approver.answer = CompletableDeferred(ApprovalOutcome.Approved(approver.sign("something else".toByteArray())))
        s.send(approvalRequest())
        scope.runCurrent()
        assertEquals("unavailable", s.drain().single().why())
    }

    @Test fun aKeyReplacedSinceHelloIsUnavailable() {
        val (s, _) = session()
        approver.keyPair = FakeApprover.newKeyPair() // e.g. fingerprints changed
        assertEquals("unavailable", s.send(approvalRequest()).single().why())
        assertTrue(approver.requests.isEmpty())
        // A new hello announces the new key, and approvals work again.
        val (fresh, hello) = session()
        assertEquals(ApprovalKeyInfo.of(approver.keyPair.public.encoded).pin, hello["approval"]!!.jsonObject["pin"]!!.jsonPrimitive.content)
        assertTrue(fresh.send(approvalRequest()).isEmpty())
    }

    @Test fun approverFailureIsUnavailable() {
        val (s, _) = session()
        approver.answer.completeExceptionally(IllegalStateException("keystore exploded"))
        s.send(approvalRequest())
        scope.runCurrent()
        assertEquals("unavailable", s.drain().single().why())
    }
}
