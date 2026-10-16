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
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.test.TestScope
import kotlinx.coroutines.test.runCurrent
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import java.security.KeyPairGenerator
import java.security.spec.ECGenParameterSpec

/** Number choices (APPROVAL.md §4.2) and the cool-down (§4.3), with a settable clock. */
@OptIn(ExperimentalCoroutinesApi::class)
class ApprovalCooldownTest {
    private class FakeApprover : Approver {
        val keyPair = KeyPairGenerator.getInstance("EC").apply { initialize(ECGenParameterSpec("secp256r1")) }.generateKeyPair()
        val decryptKeyPair = KeyPairGenerator.getInstance("RSA").apply { initialize(2048) }.generateKeyPair()
        val requests = mutableListOf<ApprovalRequest>()
        val decryptions = mutableListOf<DecryptRequest>()
        var why = ApprovalWhy.DENIED

        override fun keyInfo() = ApprovalKeyInfo.of(keyPair.public.encoded)

        override fun decryptKeyInfo() = ApprovalKeyInfo.of(decryptKeyPair.public.encoded)

        override suspend fun approve(request: ApprovalRequest): ApprovalOutcome {
            requests += request
            return ApprovalOutcome.Refused(why)
        }

        override suspend fun decrypt(request: DecryptRequest): DecryptOutcome {
            decryptions += request
            return DecryptOutcome.Refused(why)
        }
    }

    private val pc = PcInfo("81d7d5f3b885fd1d", "arch-desktop")
    private val otherPc = PcInfo("5c1e0e9a7d3b2f41", "laptop")
    private val tokens = listOf(pc, otherPc).associateBy { Secrets.newToken() }
    private val approver = FakeApprover()
    private val scope = TestScope()
    private val t0 = System.currentTimeMillis()
    private var now = t0
    private val service = ApprovalService(approver, scope, clock = { now })

    private val hub = BridgeHub(
        source = FakeSensorSource(),
        serverInfo = ServerInfo("phonetopc-android", "test"),
        deviceInfoProvider = { DeviceInfo("635f3a01db1859bc", "Test Phone", platform = "android") },
        security = SecureAccess(authenticate = { t -> tokens.entries.firstOrNull { TokenHash.matches(t, TokenHash.of(it.key)) }?.value }),
        approvals = service,
    )

    private fun Session.drain(): List<JsonObject> =
        generateSequence { tryReceiveOutgoing() }.map { ProtocolJson.parseToJsonElement(it).jsonObject }.toList()

    private fun Session.send(text: String): List<JsonObject> = runBlocking {
        handleText(text)
        drain()
    }

    private fun session(who: PcInfo = pc): Session = hub.openSession("pc").also { s ->
        val token = tokens.entries.single { it.value == who }.key
        s.send("""{"jsonrpc":"2.0","id":0,"method":"hello","params":{"protocol":"phonetopc/1","auth":{"token":"$token"}}}""")
    }

    private fun approval() =
        """{"jsonrpc":"2.0","id":1,"method":"request_approval","params":{"reason":"Deploy?","program":"deploy.sh",""" +
            """"nonce":"${Base64Url.encode(Secrets.bytes(32))}","match":42,"expiry":${now + 40_000}}}"""

    private val decryption get() =
        """{"jsonrpc":"2.0","id":2,"method":"decrypt","params":{"reason":"Decrypt a.age","program":"age","match":42,""" +
            """"key_pin":"${approver.decryptKeyInfo().pin}","ciphertext":"${Base64Url.encode(ByteArray(256))}"}}"""

    private fun shownCount() = approver.requests.size + approver.decryptions.size

    /** Asks at t0 + [atMillis], the user answering [why]; returns the `why` of the answer. */
    private fun ask(s: Session, atMillis: Long, why: ApprovalWhy, decrypt: Boolean = false): String {
        now = t0 + atMillis
        approver.why = why
        val shown = shownCount()
        val answer = s.send(if (decrypt) decryption else approval()).ifEmpty {
            scope.runCurrent()
            s.drain()
        }.single()
        val got = answer["result"]!!.jsonObject["why"]!!.jsonPrimitive.content
        assertEquals("shown at $atMillis", got != "cooldown", shownCount() > shown)
        return got
    }

    @Test fun choicesHoldTheMatchAndTwoOthers() {
        val s = session()
        val positions = mutableSetOf<Int>()
        repeat(60) { ask(s, 0, ApprovalWhy.LOCKOUT) }
        for (r in approver.requests) {
            assertEquals(3, r.choices.toSet().size)
            assertTrue(r.choices.all { it in 10..99 })
            positions += r.choices.indexOf(r.match)
        }
        assertEquals(setOf(0, 1, 2), positions)
    }

    @Test fun threeRefusalsWithinTenMinutesStartFiveMinutesOfCooldown() {
        val s = session()
        val minute = 60_000L
        assertEquals("denied", ask(s, 0, ApprovalWhy.DENIED))
        assertEquals("mismatch", ask(s, minute, ApprovalWhy.MISMATCH))
        // Other outcomes neither count nor reset the count.
        assertEquals("lockout", ask(s, 2 * minute, ApprovalWhy.LOCKOUT))
        assertEquals("timeout", ask(s, 2 * minute, ApprovalWhy.TIMEOUT))
        // A decryption's refusal counts too.
        assertEquals("cancelled", ask(s, 3 * minute, ApprovalWhy.CANCELLED, decrypt = true))
        assertEquals("cooldown", ask(s, 3 * minute, ApprovalWhy.DENIED))
        assertEquals("cooldown", ask(s, 3 * minute, ApprovalWhy.DENIED, decrypt = true))
        // Only this PC.
        assertEquals("denied", ask(session(otherPc), 3 * minute, ApprovalWhy.DENIED))
        assertEquals("cooldown", ask(s, 8 * minute - 1, ApprovalWhy.DENIED))
        assertEquals("lockout", ask(s, 8 * minute, ApprovalWhy.LOCKOUT))

        // The refusals before the cool-down are forgotten, and older ones than 10 minutes don't count.
        assertEquals("denied", ask(s, 9 * minute, ApprovalWhy.DENIED))
        assertEquals("denied", ask(s, 13 * minute, ApprovalWhy.DENIED))
        assertEquals("denied", ask(s, 19 * minute, ApprovalWhy.DENIED))
        assertEquals("lockout", ask(s, 19 * minute, ApprovalWhy.LOCKOUT))
        assertEquals("denied", ask(s, 20 * minute, ApprovalWhy.DENIED))
        assertEquals("cooldown", ask(s, 20 * minute, ApprovalWhy.DENIED))
    }
}
