package io.github.melker12345.phonetopc.protocol.security

import io.github.melker12345.phonetopc.protocol.conformanceDir
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.File

/** spec/conformance/vectors/vectors.json: every implementation MUST pass these (SECURITY.md §4.3). */
class VectorsTest {
    private val dir = File(conformanceDir(), "vectors")
    private val vectors: JsonObject = Json.parseToJsonElement(File(dir, "vectors.json").readText()).jsonObject

    private fun JsonObject.str(key: String) = this[key]!!.jsonPrimitive.content
    private fun hex(s: String) = s.chunked(2).map { it.toInt(16).toByte() }.toByteArray()
    private fun ByteArray.hex() = joinToString("") { "%02x".format(it) }

    @Test fun pins() {
        val cases = vectors["pin"]!!.jsonArray
        assertTrue(cases.isNotEmpty())
        for (case in cases.map { it.jsonObject }) {
            val cert = Pins.parsePemCertificate(File(dir, case.str("cert")).readText())
            assertEquals(case.str("cert"), case.str("pin"), Pins.of(cert))
            assertTrue(Pins.isValid(case.str("pin")))
        }
    }

    @Test fun base64url() {
        val v = vectors["base64url"]!!.jsonObject
        assertEquals(v.str("encoded"), Base64Url.encode(hex(v.str("bytes_hex"))))
        assertArrayEquals(hex(v.str("bytes_hex")), Base64Url.decode(v.str("encoded")))
    }

    @Test fun sas() {
        val v = vectors["sas"]!!.jsonObject
        val npc = Base64Url.decode(v.str("npc_b64url"))!!
        val nphone = Base64Url.decode(v.str("nphone_b64url"))!!
        assertEquals(32, npc.size)
        assertEquals(32, nphone.size)
        assertEquals(v.str("commitment"), Sas.commitment(npc))
        assertTrue(Sas.verifyCommitment(v.str("commitment"), npc))
        assertFalse(Sas.verifyCommitment(v.str("commitment"), nphone))
        assertEquals(v.str("sha256_hex"), Sas.digest(v.str("pc_pin"), v.str("phone_pin"), npc, nphone).hex())
        assertEquals(v.str("code"), Sas.code(v.str("pc_pin"), v.str("phone_pin"), npc, nphone))
    }

    @Test fun sasDiffersWhenAPinDiffers() {
        val v = vectors["sas"]!!.jsonObject
        val npc = Base64Url.decode(v.str("npc_b64url"))!!
        val nphone = Base64Url.decode(v.str("nphone_b64url"))!!
        // A man in the middle shows the phone a different PC pin: the code changes.
        val mitm = Sas.code(v.str("phone_pin"), v.str("phone_pin"), npc, nphone)
        assertFalse(mitm == v.str("code"))
    }
}

class CryptoTest {
    @Test fun base64urlIsStrict() {
        assertNull(Base64Url.decode("-_8A="))
        assertNull(Base64Url.decode("+/8A"))
        assertNull(Base64Url.decode("-_8 A"))
        assertNull(Base64Url.decode("A"))
        assertArrayEquals(ByteArray(0), Base64Url.decode(""))
    }

    @Test fun tokensAreRandom32Bytes() {
        val a = Secrets.newToken()
        val b = Secrets.newToken()
        assertEquals(32, Base64Url.decode(a)!!.size)
        assertEquals(43, a.length)
        assertFalse(a == b)
    }

    @Test fun tokenHashMatchesOnlyItsToken() {
        val t = Secrets.newToken()
        val h = TokenHash.of(t)
        assertTrue(TokenHash.matches(t, h))
        assertFalse(TokenHash.matches(Secrets.newToken(), h))
        assertFalse(TokenHash.matches("", h))
    }

    @Test fun constantTimeEquals() {
        assertTrue(ConstantTime.equals("abc", "abc"))
        assertFalse(ConstantTime.equals("abc", "abd"))
        assertFalse(ConstantTime.equals("abc", "abcd"))
    }

    @Test fun sasFormatting() {
        assertEquals("940 411", Sas.format("940411"))
        assertEquals("004 211", Sas.format("004211"))
    }

    @Test fun shortHexGroupsPinBytes() {
        // 0xfb 0xff 0x00 ... from the base64url vector, padded to 32 bytes.
        val pin = Pins.PREFIX + Base64Url.encode(byteArrayOf(0xfb.toByte(), 0xff.toByte(), 0) + ByteArray(29))
        assertEquals("FBFF 0000", Pins.shortHex(pin, bytes = 4))
    }

    @Test fun rateLimiterBlocksAfterFailuresAndRecovers() {
        var now = 0L
        val limiter = HelloRateLimiter(maxFailures = 3, windowMillis = 1_000, clock = { now })
        repeat(2) { limiter.recordFailure("1.2.3.4") }
        assertFalse(limiter.isBlocked("1.2.3.4"))
        limiter.recordFailure("1.2.3.4")
        assertTrue(limiter.isBlocked("1.2.3.4"))
        assertFalse(limiter.isBlocked("5.6.7.8"))
        now = 999
        assertTrue(limiter.isBlocked("1.2.3.4"))
        now = 1_000
        assertFalse(limiter.isBlocked("1.2.3.4"))
    }
}
