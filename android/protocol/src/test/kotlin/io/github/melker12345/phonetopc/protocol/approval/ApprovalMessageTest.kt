package io.github.melker12345.phonetopc.protocol.approval

import io.github.melker12345.phonetopc.protocol.RpcError
import io.github.melker12345.phonetopc.protocol.RpcException
import io.github.melker12345.phonetopc.protocol.conformanceDir
import io.github.melker12345.phonetopc.protocol.security.Base64Url
import io.github.melker12345.phonetopc.protocol.security.Pem
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Test
import java.io.File
import java.security.Signature

/** APPROVAL.md §4 params and §5 message, against spec/conformance/vectors/approval.json. */
class ApprovalMessageTest {
    private val vectors = Json.parseToJsonElement(File(conformanceDir(), "vectors/approval.json").readText()).jsonObject

    private fun JsonObject.s(key: String) = this[key]!!.jsonPrimitive.content

    private fun message(v: JsonObject) = ApprovalMessage.build(v.s("phone_id"), v.s("pc_id"), v.s("nonce"), v.s("reason"))

    @Test fun messageBytesMatchTheVector() {
        val valid = vectors["valid"]!!.jsonObject
        val bytes = message(valid)
        assertEquals(vectors.s("message_utf8"), String(bytes, Charsets.UTF_8))
        assertEquals(vectors.s("message_hex"), bytes.joinToString("") { "%02x".format(it) })
        assertFalse("no trailing newline", bytes.last() == '\n'.code.toByte())
    }

    @Test fun keyAndPinMatchTheVector() {
        val info = ApprovalKeyInfo.of(Base64Url.decode(vectors.s("key"))!!)
        assertEquals(vectors.s("key"), info.key)
        assertEquals(vectors.s("pin"), info.pin)
    }

    @Test fun acceptsTheValidSignature() {
        val valid = vectors["valid"]!!.jsonObject
        assertTrue(ApprovalMessage.verify(valid.s("key"), message(valid), valid.s("signature")))
    }

    @Test fun rejectsEveryTamperedVariant() {
        val invalid = vectors["invalid"]!!.jsonArray.map { it.jsonObject }
        assertTrue(invalid.size >= 6)
        for (v in invalid) {
            // Built from the verifier's own values, never from echoed ones (§5).
            assertFalse(v.s("name"), ApprovalMessage.verify(v.s("key"), message(v), v.s("signature")))
        }
    }

    @Test fun rejectsMalformedInput() {
        val valid = vectors["valid"]!!.jsonObject
        assertFalse(ApprovalMessage.verify("not a key", message(valid), valid.s("signature")))
        assertFalse(ApprovalMessage.verify(valid.s("key"), message(valid), "!!"))
        assertFalse(ApprovalMessage.verify(valid.s("key"), message(valid), ""))
    }

    @Test fun signsWithTheTestKeyLikeThePhone() {
        val valid = vectors["valid"]!!.jsonObject
        val key = Pem.ecPrivateKey(File(conformanceDir(), "vectors/approval-key.pem").readText())
        val sig = Signature.getInstance(ApprovalMessage.ALGORITHM).run {
            initSign(key)
            update(message(valid))
            sign()
        }
        assertTrue(ApprovalMessage.verify(valid.s("key"), message(valid), Base64Url.encode(sig)))
    }

    @Test fun hashesTheReasonAsUtf8() {
        val bytes = ApprovalMessage.build("p", "c", "n", "Kör deploy.sh? ✓")
        val last = String(bytes, Charsets.UTF_8).substringAfterLast('\n')
        val expected = Base64Url.encode(
            java.security.MessageDigest.getInstance("SHA-256").digest("Kör deploy.sh? ✓".toByteArray(Charsets.UTF_8)),
        )
        assertEquals(expected, last)
    }

    // ---- params (§4) ----

    private val nonce = Base64Url.encode(ByteArray(32) { it.toByte() })

    private fun parse(json: String) = ApprovalParams.parse(Json.parseToJsonElement(json).jsonObject)

    private fun assertInvalid(json: String) {
        try {
            parse(json)
            fail("accepted $json")
        } catch (e: RpcException) {
            assertEquals(json, RpcError.INVALID_PARAMS, e.error)
        }
    }

    @Test fun paramsDefaultsAndBounds() {
        val p = parse("""{"reason":"Run deploy.sh?","nonce":"$nonce"}""")
        assertEquals("Run deploy.sh?", p.reason)
        assertEquals(nonce, p.nonce)
        assertEquals(30_000L, p.timeoutMillis)
        assertEquals(5_000L, parse("""{"reason":"x","nonce":"$nonce","timeout_ms":5000}""").timeoutMillis)
        assertEquals(120_000L, parse("""{"reason":"x","nonce":"$nonce","timeout_ms":120000}""").timeoutMillis)
        assertEquals(30_000L, parse("""{"reason":"x","nonce":"$nonce","timeout_ms":null}""").timeoutMillis)
        // Unknown members are ignored, as everywhere in the protocol.
        parse("""{"reason":"x","nonce":"$nonce","extra":1}""")
    }

    @Test fun reasonIsKeptVerbatimButMeasuredTrimmed() {
        assertEquals("  padded  ", parse("""{"reason":"  padded  ","nonce":"$nonce"}""").reason)
        val max = "a".repeat(200)
        parse("""{"reason":"   $max   ","nonce":"$nonce"}""")
        // 200 characters that are each two UTF-16 units still count as 200.
        parse("""{"reason":"${"😀".repeat(200)}","nonce":"$nonce"}""")
        assertInvalid("""{"reason":"${"😀".repeat(201)}","nonce":"$nonce"}""")
    }

    @Test fun invalidParams() {
        listOf(
            """{"nonce":"$nonce"}""",
            """{"reason":null,"nonce":"$nonce"}""",
            """{"reason":5,"nonce":"$nonce"}""",
            """{"reason":"","nonce":"$nonce"}""",
            """{"reason":"   ","nonce":"$nonce"}""",
            """{"reason":"${"a".repeat(201)}","nonce":"$nonce"}""",
            """{"reason":"line\nbreak","nonce":"$nonce"}""",
            """{"reason":"tab\there","nonce":"$nonce"}""",
            """{"reason":"bell\u0007","nonce":"$nonce"}""",
            """{"reason":"del\u007f","nonce":"$nonce"}""",
            """{"reason":"c1\u0085","nonce":"$nonce"}""",
            """{"reason":"lone \ud800 surrogate","nonce":"$nonce"}""",
            """{"reason":"x"}""",
            """{"reason":"x","nonce":5}""",
            """{"reason":"x","nonce":"${Base64Url.encode(ByteArray(31))}"}""",
            """{"reason":"x","nonce":"${Base64Url.encode(ByteArray(33))}"}""",
            """{"reason":"x","nonce":"${nonce.dropLast(1)}+"}""",
            """{"reason":"x","nonce":"$nonce="}""",
            """{"reason":"x","nonce":"$nonce","timeout_ms":4999}""",
            """{"reason":"x","nonce":"$nonce","timeout_ms":120001}""",
            """{"reason":"x","nonce":"$nonce","timeout_ms":30000.5}""",
            """{"reason":"x","nonce":"$nonce","timeout_ms":3e4}""",
            """{"reason":"x","nonce":"$nonce","timeout_ms":"30000"}""",
            """{"reason":"x","nonce":"$nonce","timeout_ms":-1}""",
        ).forEach(::assertInvalid)
    }

    @Test fun outcomesSerializeLikeTheSchema() {
        assertEquals(
            """{"approved":true,"signature":"AQI"}""",
            ApprovalOutcome.Approved(byteArrayOf(1, 2)).toJson().toString(),
        )
        assertEquals(
            listOf("denied", "cancelled", "timeout", "busy", "lockout", "unavailable"),
            ApprovalWhy.entries.map { it.wire },
        )
        assertEquals("""{"approved":false,"why":"lockout"}""", ApprovalOutcome.Refused(ApprovalWhy.LOCKOUT).toJson().toString())
        assertArrayEquals(byteArrayOf(1, 2), (ApprovalOutcome.Approved(byteArrayOf(1, 2))).signature)
    }
}
