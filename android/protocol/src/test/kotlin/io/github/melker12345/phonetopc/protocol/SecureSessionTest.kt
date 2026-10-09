package io.github.melker12345.phonetopc.protocol

import io.github.melker12345.phonetopc.protocol.security.HelloRateLimiter
import io.github.melker12345.phonetopc.protocol.security.Secrets
import io.github.melker12345.phonetopc.protocol.security.TokenHash
import kotlinx.coroutines.runBlocking
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.int
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/** SECURITY.md §5: authenticated hello in secure mode. */
class SecureSessionTest {
    private val token = Secrets.newToken()
    private val paired = mutableMapOf(TokenHash.of(token) to PcInfo("9d1c0a", "arch-desktop"))
    private val authenticated = mutableListOf<PcInfo>()
    private var now = 0L

    private val hub = BridgeHub(
        source = FakeSensorSource(),
        serverInfo = ServerInfo("phonetopc-android", "0.2.0"),
        deviceInfoProvider = { DeviceInfo("dev1", "Test Phone", platform = "android") },
        security = SecureAccess(
            authenticate = { t -> paired.entries.firstOrNull { TokenHash.matches(t, it.key) }?.value },
            limiter = HelloRateLimiter(maxFailures = 3, windowMillis = 60_000, clock = { now }),
            onAuthenticated = { authenticated += it },
        ),
    )

    private fun Session.drain(): List<JsonObject> =
        generateSequence { tryReceiveOutgoing() }.map { ProtocolJson.parseToJsonElement(it).jsonObject }.toList()

    private fun Session.send(text: String): List<JsonObject> = runBlocking {
        handleText(text)
        drain()
    }

    private fun Session.hello(auth: String?): List<JsonObject> {
        val a = auth?.let { ""","auth":$it""" } ?: ""
        return send("""{"jsonrpc":"2.0","id":1,"method":"hello","params":{"protocol":"phonetopc/1"$a}}""")
    }

    private fun JsonObject.errorCode(): Int? = this["error"]?.jsonObject?.get("code")?.jsonPrimitive?.int

    private fun assertUnauthorizedAndClosed(s: Session, out: List<JsonObject>) {
        assertEquals(1, out.size)
        assertEquals(-32010, out.single().errorCode())
        assertEquals("unauthorized", out.single()["error"]!!.jsonObject["data"]!!.jsonObject["name"]!!.jsonPrimitive.content)
        assertTrue("close requested after the error", s.closeRequest.isCompleted)
        assertEquals(CloseCodes.POLICY_VIOLATION, s.closeRequest.getCompleted().code)
        assertFalse(s.helloDone)
    }

    @Test fun validTokenSucceedsWithPcInResult() {
        val s = hub.openSession("192.168.1.40:5000", "192.168.1.40")
        val r = s.hello("""{"token":"$token"}""").single()
        val pc = r["result"]!!.jsonObject["pc"]!!.jsonObject
        assertEquals("9d1c0a", pc["id"]!!.jsonPrimitive.content)
        assertEquals("arch-desktop", pc["name"]!!.jsonPrimitive.content)
        assertTrue(s.helloDone)
        assertEquals(PcInfo("9d1c0a", "arch-desktop"), s.pc)
        assertEquals(listOf(PcInfo("9d1c0a", "arch-desktop")), authenticated)
        assertFalse(s.closeRequest.isCompleted)
        assertEquals("9d1c0a", hub.clients.value.single().pc?.id)
    }

    @Test fun missingAuthIsUnauthorized() {
        val s = hub.openSession("a")
        assertUnauthorizedAndClosed(s, s.hello(null))
    }

    @Test fun unknownTokenIsUnauthorized() {
        val s = hub.openSession("a")
        assertUnauthorizedAndClosed(s, s.hello("""{"token":"${Secrets.newToken()}"}"""))
    }

    @Test fun malformedAuthIsUnauthorized() {
        for (auth in listOf("5", """{"token":5}""", """{"token":""}""", """{}""", "null")) {
            val s = hub.openSession("a-$auth")
            assertUnauthorizedAndClosed(s, s.hello(auth))
        }
    }

    @Test fun framesAfterRejectionAreIgnored() {
        val s = hub.openSession("a")
        s.hello(null)
        assertTrue(s.send("""{"jsonrpc":"2.0","id":2,"method":"list_sensors"}""").isEmpty())
    }

    @Test fun requestsBeforeHelloStillGetHelloRequired() {
        val s = hub.openSession("a")
        val r = s.send("""{"jsonrpc":"2.0","id":2,"method":"list_sensors"}""").single()
        assertEquals(-32003, r.errorCode())
        assertFalse(s.closeRequest.isCompleted)
    }

    @Test fun unsupportedProtocolComesBeforeAuth() {
        val s = hub.openSession("a")
        val r = s.send("""{"jsonrpc":"2.0","id":1,"method":"hello","params":{"protocol":"phonetopc/9"}}""").single()
        assertEquals(-32004, r.errorCode())
        assertFalse(s.closeRequest.isCompleted)
    }

    @Test fun revokedTokenIsUnauthorized() {
        paired.clear()
        val s = hub.openSession("a")
        assertUnauthorizedAndClosed(s, s.hello("""{"token":"$token"}"""))
    }

    @Test fun rateLimitPerAddressBlocksEvenValidTokens() {
        repeat(3) {
            val bad = hub.openSession("10.0.0.9:$it", "10.0.0.9")
            assertUnauthorizedAndClosed(bad, bad.hello(null))
        }
        val blocked = hub.openSession("10.0.0.9:99", "10.0.0.9")
        assertUnauthorizedAndClosed(blocked, blocked.hello("""{"token":"$token"}"""))
        // Another address is unaffected.
        val other = hub.openSession("10.0.0.10:1", "10.0.0.10")
        assertTrue("result" in other.hello("""{"token":"$token"}""").single())
        // The window passes.
        now += 60_000
        val later = hub.openSession("10.0.0.9:100", "10.0.0.9")
        assertTrue("result" in later.hello("""{"token":"$token"}""").single())
    }

    @Test fun revokingAPcClosesItsSessions() {
        val s = hub.openSession("a")
        s.hello("""{"token":"$token"}""")
        val other = hub.openSession("b")
        assertEquals(1, hub.disconnectPcs { it.id == "9d1c0a" })
        assertEquals(CloseCodes.POLICY_VIOLATION, s.closeRequest.getCompleted().code)
        assertFalse(other.closeRequest.isCompleted)
        assertTrue(s.send("""{"jsonrpc":"2.0","id":3,"method":"list_sensors"}""").isEmpty())
    }

    @Test fun devModeIgnoresAuth() {
        val dev = BridgeHub(FakeSensorSource(), ServerInfo("x", "1"), { DeviceInfo("d", "P", platform = "android") })
        val s = dev.openSession("a")
        val r = s.hello("""{"token":"whatever"}""").single()
        assertNull(r["result"]!!.jsonObject["pc"])
        assertTrue(s.helloDone)
    }
}
