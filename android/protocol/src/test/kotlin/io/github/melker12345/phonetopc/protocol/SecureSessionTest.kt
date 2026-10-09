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

    @Test fun authComesBeforeProtocol() {
        // SECURITY.md §5: params shape, then auth, then protocol.
        val s = hub.openSession("a")
        assertUnauthorizedAndClosed(s, s.send("""{"jsonrpc":"2.0","id":1,"method":"hello","params":{"protocol":"phonetopc/9"}}"""))
        val ok = hub.openSession("b")
        val r = ok.send("""{"jsonrpc":"2.0","id":1,"method":"hello","params":{"protocol":"phonetopc/9","auth":{"token":"$token"}}}""").single()
        assertEquals(-32004, r.errorCode())
        assertFalse(ok.closeRequest.isCompleted)
        assertFalse(ok.helloDone)
    }

    @Test fun paramsShapeComesBeforeAuth() {
        for (params in listOf("""["phonetopc/1"]""", """{"auth":{"token":"$token"}}""", """{"protocol":1}""", """{}""")) {
            val s = hub.openSession("a")
            val r = s.send("""{"jsonrpc":"2.0","id":1,"method":"hello","params":$params}""").single()
            assertEquals(params, -32602, r.errorCode())
            assertFalse(params, s.closeRequest.isCompleted)
        }
    }

    @Test fun failedRehelloCloses1008() {
        val bad = listOf(
            """{"protocol":"phonetopc/1","auth":{"token":"${Secrets.newToken()}"}}""",
            """{"protocol":"phonetopc/9","auth":{"token":"$token"}}""",
            """{"auth":{"token":"$token"}}""",
        )
        for (params in bad) {
            val s = hub.openSession("a")
            assertTrue("result" in s.hello("""{"token":"$token"}""").single())
            val out = s.send("""{"jsonrpc":"2.0","id":2,"method":"hello","params":$params}""")
            assertTrue(params, "error" in out.single())
            assertEquals(params, CloseCodes.POLICY_VIOLATION, s.closeRequest.getCompleted().code)
            assertTrue(s.send("""{"jsonrpc":"2.0","id":3,"method":"list_sensors"}""").isEmpty())
        }
    }

    @Test fun successfulRehelloKeepsTheSession() {
        val s = hub.openSession("a")
        s.hello("""{"token":"$token"}""")
        assertTrue("result" in s.hello("""{"token":"$token"}""").single())
        assertFalse(s.closeRequest.isCompleted)
    }

    @Test fun revokedTokenIsUnauthorized() {
        paired.clear()
        val s = hub.openSession("a")
        assertUnauthorizedAndClosed(s, s.hello("""{"token":"$token"}"""))
    }

    @Test fun rateLimitPerAddressCloses1013EvenWithValidTokens() {
        repeat(3) {
            val bad = hub.openSession("10.0.0.9:$it", "10.0.0.9")
            assertUnauthorizedAndClosed(bad, bad.hello(null))
        }
        // A new connection from that address is closed with 1013 at once ...
        val blocked = hub.openSession("10.0.0.9:99", "10.0.0.9")
        assertEquals(CloseCodes.TRY_AGAIN_LATER, blocked.closeRequest.getCompleted().code)
        // ... and a hello on it gets no answer, even with a valid token.
        assertTrue(blocked.hello("""{"token":"$token"}""").isEmpty())
        assertFalse(blocked.helloDone)
        // Another address is unaffected.
        val other = hub.openSession("10.0.0.10:1", "10.0.0.10")
        assertTrue("result" in other.hello("""{"token":"$token"}""").single())
        // The window passes.
        now += 60_000
        val later = hub.openSession("10.0.0.9:100", "10.0.0.9")
        assertFalse(later.closeRequest.isCompleted)
        assertTrue("result" in later.hello("""{"token":"$token"}""").single())
    }

    @Test fun limitReachedWhileConnectedClosesAtHello() {
        val waiting = hub.openSession("10.0.0.9:1", "10.0.0.9")
        repeat(3) {
            val bad = hub.openSession("10.0.0.9:${it + 2}", "10.0.0.9")
            bad.hello(null)
        }
        assertTrue(waiting.hello("""{"token":"$token"}""").isEmpty())
        assertEquals(CloseCodes.TRY_AGAIN_LATER, waiting.closeRequest.getCompleted().code)
    }

    @Test fun repairingClosesOnlySessionsWithTheOldToken() {
        val old = hub.openSession("a")
        old.hello("""{"token":"$token"}""")
        val newToken = Secrets.newToken()
        paired.clear()
        paired[TokenHash.of(newToken)] = PcInfo("9d1c0a", "arch-desktop")
        val fresh = hub.openSession("b")
        fresh.hello("""{"token":"$newToken"}""")
        val current = TokenHash.of(newToken)
        assertEquals(1, hub.disconnectSessions { pc, hash -> pc.id == "9d1c0a" && hash != current })
        assertEquals(CloseCodes.POLICY_VIOLATION, old.closeRequest.getCompleted().code)
        assertFalse(fresh.closeRequest.isCompleted)
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
