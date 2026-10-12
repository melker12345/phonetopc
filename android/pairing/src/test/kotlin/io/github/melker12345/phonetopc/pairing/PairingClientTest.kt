package io.github.melker12345.phonetopc.pairing

import io.github.melker12345.phonetopc.protocol.security.Base64Url
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.launch
import kotlinx.coroutines.test.TestScope
import kotlinx.coroutines.test.advanceTimeBy
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonPrimitive
import kotlinx.serialization.json.int
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import kotlin.time.Duration.Companion.minutes
import kotlin.time.Duration.Companion.seconds

@OptIn(ExperimentalCoroutinesApi::class)
class PairingClientTest {
    private val phone = PhoneInfo("635f3a01db1859bc", "Test Phone", FakePc.PHONE_PIN, 8765, listOf("192.168.1.153"))
    private val stored = mutableListOf<NewPairing>()

    private fun client(pc: FakePc) = PairingClient(pc, phone = { phone }, store = { stored += it })

    private fun failure(c: PairingClient) = (c.state.value as PairingState.Failed).failure

    // ---- way A: QR ----

    @Test fun qrHappyPath() = runTest {
        val pc = FakePc()
        val c = client(pc)
        c.pairWithQr(pc.qr())
        assertEquals(PairingState.Paired(pc.id, pc.name), c.state.value)
        assertEquals(listOf("pair_qr", "pair_finish"), pc.calls)
        val p = stored.single()
        assertEquals(pc.id, p.pcId)
        assertEquals(pc.name, p.pcName)
        assertEquals(pc.pin, p.pin)
        assertEquals(pc.storedToken, p.token)
        assertEquals(32, Base64Url.decode(p.token)!!.size)
        // The phone verified the pin while connecting.
        assertEquals(pc.pin, pc.connects.single().second)
        val seen = pc.phoneSeen!!
        assertEquals(phone.id, seen["id"]!!.jsonPrimitive.content)
        assertEquals(phone.pin, seen["pin"]!!.jsonPrimitive.content)
        assertEquals(8765, seen["port"]!!.jsonPrimitive.int)
        assertEquals("192.168.1.153", seen["addresses"]!!.jsonArray.single().jsonPrimitive.content)
    }

    @Test fun qrTriesEachHostInOrder() = runTest {
        val pc = FakePc(reachable = setOf("10.0.0.2"))
        val c = client(pc)
        c.pairWithQr(pc.qr(hosts = listOf("10.0.0.1", "fd00::2", "10.0.0.2")))
        assertTrue(c.state.value is PairingState.Paired)
        assertEquals(listOf("10.0.0.1", "fd00::2", "10.0.0.2"), pc.connects.map { it.first })
    }

    @Test fun qrPinMismatchIsAHardFailure() = runTest {
        val pc = FakePc(presentedPin = FakePc.MITM_PIN)
        val c = client(pc)
        c.pairWithQr(pc.qr())
        assertEquals(PairingFailure.PinMismatch, failure(c))
        assertTrue(pc.calls.isEmpty())
        assertTrue(stored.isEmpty())
    }

    @Test fun qrSkipsAHostWithAnotherPin() = runTest {
        // SECURITY.md §4.2: a host with a different pin is skipped like an unreachable one.
        val pc = FakePc(reachable = setOf("10.0.0.1", "10.0.0.2"), pinByHost = mapOf("10.0.0.1" to FakePc.MITM_PIN))
        val c = client(pc)
        c.pairWithQr(pc.qr(hosts = listOf("10.0.0.1", "10.0.0.2")))
        assertEquals(PairingState.Paired(pc.id, pc.name), c.state.value)
        assertEquals(listOf("10.0.0.1", "10.0.0.2"), pc.connects.map { it.first })
        assertEquals(pc.pin, stored.single().pin)
    }

    @Test fun qrWaitsAtLeastFiveMinutesForThePcUser() = runTest {
        // SECURITY.md §4.1: long answers get at least 5 minutes by default.
        val pc = FakePc().apply { neverAnswerApproval = true }
        val job = launch { client(pc).pairWithQr(pc.qr()) }
        advanceTimeBy(5.minutes)
        runCurrent()
        assertTrue("gave up before 5 minutes", job.isActive)
        job.cancel()
    }

    @Test fun qrUnreachable() = runTest {
        val pc = FakePc(reachable = emptySet())
        val c = client(pc)
        c.pairWithQr(pc.qr(hosts = listOf("10.0.0.1", "10.0.0.2")))
        assertTrue(failure(c) is PairingFailure.Unreachable)
    }

    @Test fun qrBadSecret() = runTest {
        val pc = FakePc()
        val c = client(pc)
        c.pairWithQr(pc.qr(qrSecret = Base64Url.encode(ByteArray(16))))
        assertEquals(PairingFailure.BadSecret, failure(c))
        assertTrue(stored.isEmpty())
    }

    @Test fun qrRejectedByPcUser() = runTest {
        val pc = FakePc().apply { userApproves = false }
        val c = client(pc)
        c.pairWithQr(pc.qr())
        assertEquals(PairingFailure.Rejected, failure(c))
        assertNull(pc.storedToken)
        assertTrue(stored.isEmpty())
    }

    @Test fun qrWaitsForApprovalThenTimesOut() = runTest {
        val pc = FakePc().apply { neverAnswerApproval = true }
        val c = PairingClient(pc, { phone }, { stored += it }, userTimeout = 2.minutes)
        val job = launch { c.pairWithQr(pc.qr()) }
        runCurrent()
        assertEquals(PairingState.WaitingForPcApproval(pc.name), c.state.value)
        advanceTimeBy(2.minutes + 1.seconds)
        job.join()
        assertEquals(PairingFailure.TimedOut, failure(c))
    }

    @Test fun nothingIsStoredWhenFinishFails() = runTest {
        val pc = FakePc().apply { errorFor = "pair_finish" to PairingErrors.EXPIRED }
        val c = client(pc)
        c.pairWithQr(pc.qr())
        assertEquals(PairingFailure.Expired, failure(c))
        assertTrue(stored.isEmpty())
    }

    @Test fun connectionLostMidway() = runTest {
        val pc = FakePc().apply { dropAfter = "pair_finish" }
        val c = client(pc)
        c.pairWithQr(pc.qr())
        assertTrue(failure(c) is PairingFailure.ConnectionLost)
        assertTrue(stored.isEmpty())
    }

    @Test fun errorCodesMapToFailures() {
        fun f(code: Int) = PairingClient.failureFor(PairingRpcException(code, "x"))
        assertEquals(PairingFailure.BadSecret, f(-32020))
        assertEquals(PairingFailure.Rejected, f(-32021))
        assertEquals(PairingFailure.OutOfOrder, f(-32022))
        assertEquals(PairingFailure.CodesDiffer, f(-32023))
        assertEquals(PairingFailure.Expired, f(-32024))
        assertEquals(PairingFailure.UnsupportedProtocol, f(-32004))
        assertTrue(f(-32603) is PairingFailure.Protocol)
    }

    // ---- way B: network + code ----

    private fun TestScope.startSas(pc: FakePc, c: PairingClient) =
        launch { c.pairOverNetwork(NetworkPc(pc.name, pc.reachable.toList(), 40123)) }

    @Test fun sasHappyPath() = runTest {
        val pc = FakePc()
        val c = client(pc)
        val job = startSas(pc, c)
        val compare = c.state.first { it is PairingState.CompareCode } as PairingState.CompareCode
        // Both sides compute the same code from the same pins and nonces.
        assertEquals(pc.pcCode, compare.code)
        assertEquals(6, compare.code.length)
        assertNull("connected without a pin", pc.connects.single().second)
        c.answerCode(true)
        job.join()
        assertEquals(PairingState.Paired(pc.id, pc.name), c.state.value)
        assertEquals(listOf("pair_sas_begin", "pair_sas_nonce", "pair_sas_confirm", "pair_finish"), pc.calls)
        assertEquals(true, pc.sasConfirmFromPhone)
        // The pin the phone saw becomes the stored pin.
        assertEquals(pc.presentedPin, stored.single().pin)
        assertEquals(pc.storedToken, stored.single().token)
    }

    @Test fun sasManInTheMiddleGetsDifferentCodes() = runTest {
        // The phone talks to a MITM presenting its own certificate, which relays to the real PC.
        val pc = FakePc(presentedPin = FakePc.MITM_PIN)
        val c = client(pc)
        val job = startSas(pc, c)
        val compare = c.state.first { it is PairingState.CompareCode } as PairingState.CompareCode
        assertTrue(compare.code != pc.pcCode)
        c.answerCode(false)
        job.join()
        assertEquals(PairingFailure.CodesDiffer, failure(c))
        assertEquals(false, pc.sasConfirmFromPhone)
        assertTrue(stored.isEmpty())
    }

    @Test fun sasPcUserSaysNo() = runTest {
        val pc = FakePc().apply { userConfirmsCode = false }
        val c = client(pc)
        val job = startSas(pc, c)
        c.state.first { it is PairingState.CompareCode }
        c.answerCode(true)
        job.join()
        assertEquals(PairingFailure.CodesDiffer, failure(c))
        assertTrue(stored.isEmpty())
    }

    @Test fun sasCommitmentMismatchAborts() = runTest {
        val pc = FakePc().apply { cheatCommitment = true }
        val c = client(pc)
        startSas(pc, c).join()
        assertEquals(PairingFailure.CommitmentMismatch, failure(c))
        // The code is never shown, and the PC is told to discard.
        assertEquals(false, pc.sasConfirmFromPhone)
        assertTrue("pair_finish" !in pc.calls)
        assertTrue(stored.isEmpty())
    }

    @Test fun sasMalformedNonceIsACommitmentFailure() = runTest {
        val pc = FakePc().apply { badNonce = true }
        val c = client(pc)
        startSas(pc, c).join()
        assertEquals(PairingFailure.CommitmentMismatch, failure(c))
        // match:false first, then abort (SECURITY.md §4.3).
        assertEquals(false, pc.sasConfirmFromPhone)
        assertEquals("pair_sas_confirm", pc.calls.last())
        assertTrue(stored.isEmpty())
    }

    @Test fun sasCancelReturnsToIdle() = runTest {
        val pc = FakePc()
        val c = client(pc)
        val job = startSas(pc, c)
        c.state.first { it is PairingState.CompareCode }
        job.cancel()
        job.join()
        assertEquals(PairingState.Idle, c.state.value)
        assertTrue(stored.isEmpty())
    }

    @Test fun sasRefusedByAQrOnlyPc() = runTest {
        val pc = FakePc().apply { errorFor = "pair_sas_begin" to PairingErrors.REJECTED }
        val c = client(pc)
        startSas(pc, c).join()
        assertEquals(PairingFailure.QrOnly, failure(c))
        assertTrue(stored.isEmpty())
    }

    @Test fun sasUnsupportedProtocol() = runTest {
        val pc = FakePc().apply { errorFor = "pair_sas_begin" to PairingErrors.UNSUPPORTED_PROTOCOL }
        val c = client(pc)
        startSas(pc, c).join()
        assertEquals(PairingFailure.UnsupportedProtocol, failure(c))
    }
}
