package io.github.melker12345.phonetopc.sensors

import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.add
import kotlinx.serialization.json.addJsonObject
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put
import kotlinx.serialization.json.putJsonArray
import java.nio.ByteBuffer
import java.nio.charset.CharacterCodingException
import java.nio.charset.CodingErrorAction
import java.nio.charset.StandardCharsets

/**
 * The `nfc`/`tag` value (spec §6) from a tag's raw data. No Android classes, so it runs in
 * plain JVM unit tests. Tags, and on Android the intents that carry them, are untrusted input:
 * everything is bounded.
 */
object NfcValues {
    class Record(val tnf: Int, val type: ByteArray, val payload: ByteArray)

    const val MAX_RECORDS = 8
    const val MAX_PAYLOAD_BYTES = 4096
    const val MAX_ID_BYTES = 32
    const val MAX_TECHS = 16
    private const val MAX_TECH_NAME = 64
    private const val MAX_TYPE_BYTES = 255

    const val TNF_WELL_KNOWN = 1
    const val TNF_ABSOLUTE_URI = 3

    /**
     * @param techList the tag's technologies, as Android's fully qualified class names
     *   (`android.nfc.tech.NfcA`) or short names.
     * @param records the tag's NDEF message, or `null` if it has none.
     */
    fun tag(id: ByteArray, techList: List<String>, records: List<Record>?): JsonElement = buildJsonObject {
        put("id", hex(id.copyOf(minOf(id.size, MAX_ID_BYTES))))
        putJsonArray("tech") {
            techList.take(MAX_TECHS).forEach { add(it.substringAfterLast('.').take(MAX_TECH_NAME)) }
        }
        if (records != null) {
            putJsonArray("ndef") {
                records.take(MAX_RECORDS).forEach { r -> addJsonObject { record(r) } }
            }
        }
    }

    private fun kotlinx.serialization.json.JsonObjectBuilder.record(r: Record) {
        val type = r.type.copyOf(minOf(r.type.size, MAX_TYPE_BYTES))
        val payload = r.payload.copyOf(minOf(r.payload.size, MAX_PAYLOAD_BYTES))
        val typeText = printableUtf8(type)
        put("tnf", r.tnf and 0x07)
        put("type", typeText ?: hex(type))
        if (r.tnf == TNF_WELL_KNOWN && typeText == "T") text(payload)?.let { put("text", it) }
        if (r.tnf == TNF_WELL_KNOWN && typeText == "U") uri(payload)?.let { put("uri", it) }
        if (r.tnf == TNF_ABSOLUTE_URI && typeText != null) put("uri", typeText)
        put("payload", hex(payload))
    }

    /** NFC Forum Text RTD: status byte (bit 7 UTF-16, bits 0-5 language code length), code, text. */
    fun text(payload: ByteArray): String? {
        if (payload.isEmpty()) return null
        val status = payload[0].toInt()
        val langLength = status and 0x3F
        val start = 1 + langLength
        if (start > payload.size) return null
        val charset = if (status and 0x80 != 0) StandardCharsets.UTF_16 else StandardCharsets.UTF_8
        return String(payload, start, payload.size - start, charset)
    }

    /** NFC Forum URI RTD: an abbreviation code, then the rest of the URI in UTF-8. */
    fun uri(payload: ByteArray): String? {
        if (payload.isEmpty()) return null
        val prefix = URI_PREFIXES.getOrElse(payload[0].toInt() and 0xFF) { return null }
        return prefix + String(payload, 1, payload.size - 1, StandardCharsets.UTF_8)
    }

    fun hex(bytes: ByteArray): String = buildString(bytes.size * 2) {
        for (b in bytes) {
            val v = b.toInt() and 0xFF
            append(HEX[v ushr 4]).append(HEX[v and 0x0F])
        }
    }

    /** [bytes] as text if it is valid UTF-8 without control characters, else `null`. */
    private fun printableUtf8(bytes: ByteArray): String? {
        val text = try {
            StandardCharsets.UTF_8.newDecoder()
                .onMalformedInput(CodingErrorAction.REPORT)
                .onUnmappableCharacter(CodingErrorAction.REPORT)
                .decode(ByteBuffer.wrap(bytes))
                .toString()
        } catch (_: CharacterCodingException) {
            return null
        }
        return text.takeIf { t -> t.none { Character.isISOControl(it) } }
    }

    private const val HEX = "0123456789abcdef"

    /** NFC Forum URI RTD, table 3. */
    private val URI_PREFIXES = listOf(
        "", "http://www.", "https://www.", "http://", "https://", "tel:", "mailto:",
        "ftp://anonymous:anonymous@", "ftp://ftp.", "ftps://", "sftp://", "smb://", "nfs://",
        "ftp://", "dav://", "news:", "telnet://", "imap:", "rtsp://", "urn:", "pop:", "sip:",
        "sips:", "tftp:", "btspp://", "btl2cap://", "btgoep://", "tcpobex://", "irdaobex://",
        "file://", "urn:epc:id:", "urn:epc:tag:", "urn:epc:pat:", "urn:epc:raw:", "urn:epc:",
        "urn:nfc:",
    )
}
