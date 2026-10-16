package io.github.melker12345.phonetopc.protocol.approval

import io.github.melker12345.phonetopc.protocol.BridgeHub
import io.github.melker12345.phonetopc.protocol.DeviceInfo
import io.github.melker12345.phonetopc.protocol.FakeSensorSource
import io.github.melker12345.phonetopc.protocol.PcInfo
import io.github.melker12345.phonetopc.protocol.ProtocolJson
import io.github.melker12345.phonetopc.protocol.SecureAccess
import io.github.melker12345.phonetopc.protocol.ServerInfo
import io.github.melker12345.phonetopc.protocol.Session
import io.github.melker12345.phonetopc.protocol.conformanceDir
import io.github.melker12345.phonetopc.protocol.security.Base64Url
import io.github.melker12345.phonetopc.protocol.security.Pem
import io.github.melker12345.phonetopc.protocol.security.Secrets
import io.github.melker12345.phonetopc.protocol.security.TokenHash
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.test.TestScope
import kotlinx.coroutines.test.runCurrent
import kotlinx.serialization.json.Json
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
import org.junit.Assert.fail
import org.junit.Test
import java.io.File
import java.security.KeyFactory
import java.security.KeyPair
import java.security.KeyPairGenerator
import java.security.PrivateKey
import java.security.spec.ECGenParameterSpec
import java.security.spec.PKCS8EncodedKeySpec
import javax.crypto.Cipher

/** DECRYPT.md: the cipher against vectors/decrypt.json, and `decrypt` in the session. */
@OptIn(ExperimentalCoroutinesApi::class)
class DecryptTest {
    private val vectors = Json.parseToJsonElement(File(conformanceDir(), "vectors/decrypt.json").readText()).jsonObject

    private fun JsonObject.s(key: String) = this[key]!!.jsonPrimitive.content

    private fun decrypt(key: PrivateKey, ciphertext: ByteArray): ByteArray =
        Cipher.getInstance(DecryptCipher.TRANSFORMATION).run {
            init(Cipher.DECRYPT_MODE, key, DecryptCipher.OAEP)
            doFinal(ciphertext)
        }

    @Test fun cipherMatchesTheVector() {
        val pem = File(conformanceDir(), "vectors/decrypt-key.pem").readText()
        val key = KeyFactory.getInstance("RSA").generatePrivate(PKCS8EncodedKeySpec(Pem.body(pem, "PRIVATE KEY")))
        val info = ApprovalKeyInfo.of(Base64Url.decode(vectors.s("key"))!!)
        assertEquals(vectors.s("pin"), info.pin)
        assertEquals(256, DecryptCipher.ciphertextSize(info))
        assertArrayEquals(Base64Url.decode(vectors.s("plaintext")), decrypt(key, Base64Url.decode(vectors.s("ciphertext"))!!))
        for (r in vectors["reject"]!!.jsonArray.map { it.jsonObject }) {
            try {
                decrypt(key, Base64Url.decode(r.s("ciphertext"))!!)
                fail("${r.s("name")} decrypted")
            } catch (_: java.security.GeneralSecurityException) {
            }
        }
    }

    private class FakeApprover : Approver {
        var keyPair: KeyPair = newKeyPair()
        var available = true
        val requests = mutableListOf<DecryptRequest>()
        var answer = CompletableDeferred<ApprovalWhy?>() // null: the user confirmed

        private val approvalKey = KeyPairGenerator.getInstance("EC").apply { initialize(ECGenParameterSpec("secp256r1")) }.generateKeyPair()

        override fun keyInfo() = ApprovalKeyInfo.of(approvalKey.public.encoded)
        override suspend fun approve(request: ApprovalRequest) = ApprovalOutcome.Refused(ApprovalWhy.UNAVAILABLE)
        override fun decryptKeyInfo() = if (available) ApprovalKeyInfo.of(keyPair.public.encoded) else null

        override suspend fun decrypt(request: DecryptRequest): DecryptOutcome {
            requests += request
            answer.await()?.let { return DecryptOutcome.Refused(it) }
            return try {
                DecryptOutcome.Decrypted(
                    Cipher.getInstance(DecryptCipher.TRANSFORMATION).run {
                        init(Cipher.DECRYPT_MODE, keyPair.private, DecryptCipher.OAEP)
                        doFinal(request.ciphertext)
                    },
                )
            } catch (_: java.security.GeneralSecurityException) {
                DecryptOutcome.Refused(ApprovalWhy.FAILED)
            }
        }

        fun encrypt(plaintext: ByteArray, to: KeyPair = keyPair): ByteArray =
            Cipher.getInstance(DecryptCipher.TRANSFORMATION).run {
                init(Cipher.ENCRYPT_MODE, to.public, DecryptCipher.OAEP)
                doFinal(plaintext)
            }

        companion object {
            fun newKeyPair(): KeyPair = KeyPairGenerator.getInstance("RSA").apply { initialize(2048) }.generateKeyPair()
        }
    }

    private val token = Secrets.newToken()
    private val pc = PcInfo("81d7d5f3b885fd1d", "arch-desktop")
    private val otherToken = Secrets.newToken()
    private val otherPc = PcInfo("5c1e0e9a7d3b2f41", "laptop")
    private val approver = FakeApprover()
    private val scope = TestScope()
    private val service = ApprovalService(approver, scope)

    private fun hub(secure: Boolean = true) = BridgeHub(
        source = FakeSensorSource(),
        serverInfo = ServerInfo("phonetopc-android", "test"),
        deviceInfoProvider = { DeviceInfo("635f3a01db1859bc", "Test Phone", platform = "android") },
        security = if (secure) SecureAccess(
            authenticate = { t ->
                when {
                    TokenHash.matches(t, TokenHash.of(token)) -> pc
                    TokenHash.matches(t, TokenHash.of(otherToken)) -> otherPc
                    else -> null
                }
            },
        ) else null,
        approvals = service,
    )

    private fun Session.drain(): List<JsonObject> =
        generateSequence { tryReceiveOutgoing() }.map { ProtocolJson.parseToJsonElement(it).jsonObject }.toList()

    private fun Session.send(text: String): List<JsonObject> = runBlocking {
        handleText(text)
        drain()
    }

    private fun session(t: String = token): Pair<Session, JsonObject> = hub().openSession("pc").let {
        it to it.send("""{"jsonrpc":"2.0","id":0,"method":"hello","params":{"protocol":"phonetopc/1","auth":{"token":"$t"}}}""")
            .single()["result"]!!.jsonObject
    }

    private val secret = "file key 16 byte".toByteArray()
    private val pin get() = ApprovalKeyInfo.of(approver.keyPair.public.encoded).pin

    private fun request(id: Int = 9, ciphertext: ByteArray = approver.encrypt(secret), keyPin: String = pin, extra: String = "") =
        """{"jsonrpc":"2.0","id":$id,"method":"decrypt","params":{"reason":"Decrypt a.age","program":"age","match":42,"key_pin":"$keyPin",""" +
            """"ciphertext":"${Base64Url.encode(ciphertext)}"$extra}}"""

    private fun JsonObject.result() = this["result"]!!.jsonObject
    private fun JsonObject.errorCode() = this["error"]?.jsonObject?.get("code")?.jsonPrimitive?.int
    private fun JsonObject.why() = result()["why"]?.jsonPrimitive?.content

    @Test fun helloAnnouncesTheKey() {
        val (_, hello) = session()
        assertTrue("decrypt" in hello["capabilities"]!!.jsonArray.map { it.jsonPrimitive.content })
        assertEquals(pin, hello["decrypt"]!!.jsonObject["pin"]!!.jsonPrimitive.content)
    }

    @Test fun decryptsAfterTheUserConfirms() {
        val (s, _) = session()
        assertTrue(s.send(request()).isEmpty())
        scope.runCurrent()
        val req = approver.requests.single()
        assertEquals("Decrypt a.age", req.reason)
        assertEquals("age", req.program)
        assertEquals(42, req.match)
        assertTrue(42 in req.choices)
        assertEquals(pc, req.pc)
        approver.answer.complete(null)
        scope.runCurrent()
        val r = s.drain().single()
        assertTrue(r.result()["decrypted"]!!.jsonPrimitive.boolean)
        assertArrayEquals(secret, Base64Url.decode(r.result()["plaintext"]!!.jsonPrimitive.content))
    }

    @Test fun refusalsAndFailures() {
        val (s, _) = session()
        s.send(request())
        scope.runCurrent()
        approver.answer.complete(ApprovalWhy.DENIED)
        scope.runCurrent()
        assertEquals("denied", s.drain().single().why())

        approver.answer = CompletableDeferred<ApprovalWhy?>().apply { complete(null) }
        s.send(request(ciphertext = approver.encrypt(secret, FakeApprover.newKeyPair())))
        scope.runCurrent()
        assertEquals("failed", s.drain().single().why())
    }

    @Test fun anotherKeyIsUnavailableWithoutAsking() {
        val (s, _) = session()
        assertEquals("unavailable", s.send(request(keyPin = "sha256/other")).single().why())
        approver.keyPair = FakeApprover.newKeyPair() // fingerprints changed after hello
        assertEquals("unavailable", s.send(request()).single().why())
        assertTrue(approver.requests.isEmpty())
    }

    @Test fun invalidParams() {
        val (s, _) = session()
        assertEquals(-32602, s.send(request(ciphertext = ByteArray(100))).single().errorCode())
        assertEquals(-32602, s.send(request(extra = ""","timeout_ms":1""")).single().errorCode())
        assertEquals(-32602, s.send("""{"jsonrpc":"2.0","id":1,"method":"decrypt","params":{"reason":"x"}}""").single().errorCode())
        assertEquals(-32602, s.send(request().replace("\"match\":42", "\"match\":9")).single().errorCode())
        assertEquals(-32602, s.send(request().replace("\"program\":\"age\",", "")).single().errorCode())
        assertEquals(-32602, s.send(request().replace("\"match\":42,", "")).single().errorCode())
        assertTrue(approver.requests.isEmpty())
    }

    @Test fun oneRequestPerPcWithApprovals() {
        val (s, _) = session()
        s.send(request(id = 1))
        scope.runCurrent()
        assertEquals("busy", s.send(request(id = 2)).single().why())
        val nonce = Base64Url.encode(Secrets.bytes(32))
        val approval = s.send("""{"jsonrpc":"2.0","id":3,"method":"request_approval","params":{"reason":"Deploy?","program":"deploy.sh","nonce":"$nonce","match":42,"expiry":${System.currentTimeMillis() + 40_000}}}""")
        assertEquals("busy", approval.single().why())
    }

    @Test fun anotherPcWaitsItsTurn() {
        val (a, _) = session()
        val (b, _) = session(otherToken)
        a.send(request(id = 1))
        assertTrue(b.send(request(id = 2)).isEmpty())
        scope.runCurrent()
        assertEquals(listOf(pc), approver.requests.map { it.pc })
        approver.answer.complete(null)
        scope.runCurrent()
        assertTrue(a.drain().single().result()["decrypted"]!!.jsonPrimitive.boolean)
        assertTrue(b.drain().single().result()["decrypted"]!!.jsonPrimitive.boolean)
        assertEquals(listOf(pc, otherPc), approver.requests.map { it.pc })
    }

    @Test fun devModeHasNoSuchMethod() {
        val s = hub(secure = false).openSession("pc")
        val hello = s.send("""{"jsonrpc":"2.0","id":0,"method":"hello","params":{"protocol":"phonetopc/1"}}""").single().result()
        assertNull(hello["decrypt"])
        assertEquals(-32601, s.send(request()).single().errorCode())
    }
}
