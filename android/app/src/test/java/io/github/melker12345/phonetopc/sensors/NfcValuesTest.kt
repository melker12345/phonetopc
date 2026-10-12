package io.github.melker12345.phonetopc.sensors

import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Test

class NfcValuesTest {
    private fun record(tnf: Int, type: String, payload: ByteArray) = NfcValues.Record(tnf, type.toByteArray(), payload)

    @Test fun tagWithoutNdef() {
        val v = NfcValues.tag(
            byteArrayOf(0x5b, 0x1e, 0x0a, 0x9c.toByte()),
            listOf("android.nfc.tech.NfcA", "android.nfc.tech.MifareClassic"),
            null,
        )
        assertEquals("""{"id":"5b1e0a9c","tech":["NfcA","MifareClassic"]}""", v.toString())
    }

    @Test fun textAndUriRecords() {
        val v = NfcValues.tag(
            byteArrayOf(0x04, 0xa2.toByte()),
            listOf("android.nfc.tech.Ndef"),
            listOf(
                record(1, "U", byteArrayOf(0x04) + "example.com/".toByteArray()),
                record(1, "T", byteArrayOf(0x02) + "enhello".toByteArray()),
                record(3, "https://example.org/x", ByteArray(0)),
                record(2, "text/plain", "hi".toByteArray()),
            ),
        )
        assertEquals(
            """{"id":"04a2","tech":["Ndef"],"ndef":[""" +
                """{"tnf":1,"type":"U","uri":"https://example.com/","payload":"046578616d706c652e636f6d2f"},""" +
                """{"tnf":1,"type":"T","text":"hello","payload":"02656e68656c6c6f"},""" +
                """{"tnf":3,"type":"https://example.org/x","uri":"https://example.org/x","payload":""},""" +
                """{"tnf":2,"type":"text/plain","payload":"6869"}]}""",
            v.toString(),
        )
    }

    @Test fun textRecordEncodings() {
        // UTF-16 (status bit 7) with a BOM, and a 5-byte language code.
        val utf16 = byteArrayOf((0x80 or 5).toByte()) + "en-GB".toByteArray() + "hé".toByteArray(Charsets.UTF_16)
        assertEquals("hé", NfcValues.text(utf16))
        assertEquals("", NfcValues.text(byteArrayOf(0x02) + "en".toByteArray()))
        assertNull(NfcValues.text(byteArrayOf(0x05, 'e'.code.toByte()))) // language code runs past the end
        assertNull(NfcValues.text(ByteArray(0)))
    }

    @Test fun uriRecordPrefixes() {
        assertEquals("tel:+46123", NfcValues.uri(byteArrayOf(0x05) + "+46123".toByteArray()))
        assertEquals("urn:nfc:x", NfcValues.uri(byteArrayOf(0x23) + "x".toByteArray()))
        assertEquals("raw", NfcValues.uri(byteArrayOf(0x00) + "raw".toByteArray()))
        assertNull(NfcValues.uri(byteArrayOf(0x24))) // reserved code
        assertNull(NfcValues.uri(ByteArray(0)))
    }

    @Test fun binaryTypeIsHex() {
        val v = NfcValues.tag(ByteArray(0), emptyList(), listOf(NfcValues.Record(4, byteArrayOf(0x00, 0xff.toByte()), byteArrayOf(1))))
        val r = v.jsonObject["ndef"]!!.jsonArray[0].jsonObject
        assertEquals("00ff", r["type"]!!.jsonPrimitive.content)
        assertEquals("", v.jsonObject["id"]!!.jsonPrimitive.content)
        // Not text or uri: those are only decoded for well-known T and U records.
        assertEquals(setOf("tnf", "type", "payload"), r.keys)
    }

    @Test fun everythingIsBounded() {
        val big = ByteArray(10_000) { 0x41 }
        val v = NfcValues.tag(
            ByteArray(100) { 1 },
            List(40) { "android.nfc.tech.Tech$it" + "x".repeat(100) },
            List(20) { record(1, "T", byteArrayOf(0x02) + "en".toByteArray() + big) },
        ).jsonObject
        assertEquals(NfcValues.MAX_ID_BYTES * 2, v["id"]!!.jsonPrimitive.content.length)
        val tech = v["tech"] as JsonArray
        assertEquals(NfcValues.MAX_TECHS, tech.size)
        assertFalse(tech.any { it.jsonPrimitive.content.length > 64 })
        val ndef = v["ndef"] as JsonArray
        assertEquals(NfcValues.MAX_RECORDS, ndef.size)
        val first = ndef[0] as JsonObject
        assertEquals(NfcValues.MAX_PAYLOAD_BYTES * 2, first["payload"]!!.jsonPrimitive.content.length)
        // The text is decoded from the kept payload.
        assertEquals(NfcValues.MAX_PAYLOAD_BYTES - 3, first["text"]!!.jsonPrimitive.content.length)
    }
}
