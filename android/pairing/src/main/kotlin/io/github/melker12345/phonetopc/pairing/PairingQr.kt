package io.github.melker12345.phonetopc.pairing

import io.github.melker12345.phonetopc.protocol.security.Base64Url
import io.github.melker12345.phonetopc.protocol.security.Pins
import java.net.URLDecoder

/**
 * The QR code of pairing way A (SECURITY.md §4.2):
 * `phonetopc://pair?v=1&id=…&n=…&h=host1,host2&p=…&pin=…&s=…`.
 */
data class PairingQr(
    val pcId: String,
    val pcName: String,
    val hosts: List<String>,
    val port: Int,
    val pin: String,
    val secret: String,
) {
    /** Never log or display this: [secret] is in it. */
    override fun toString() = "PairingQr(pcId=$pcId, pcName=$pcName, hosts=$hosts, port=$port, pin=$pin)"

    companion object {
        const val PREFIX = "phonetopc://pair?"

        /** Why a scanned code isn't a usable pairing code. */
        enum class Problem { NOT_PHONETOPC, UNSUPPORTED_VERSION, MALFORMED }

        class Invalid(val problem: Problem, message: String) : Exception(message)

        /** Cheap check for the scanner: is this one of ours at all? */
        fun looksLikePairingUri(text: String) = text.trim().startsWith(PREFIX, ignoreCase = true)

        /**
         * Parses a scanned code. Values are percent-decoded first (so `%2C`, `%3A` and `+` as a
         * space are fine), then `h` is split on commas. Unknown parameters are ignored.
         */
        fun parse(text: String): PairingQr {
            val trimmed = text.trim()
            if (!looksLikePairingUri(trimmed)) throw Invalid(Problem.NOT_PHONETOPC, "not a phonetopc pairing code")
            val query = trimmed.substring(PREFIX.length).substringBefore('#')
            val params = HashMap<String, String>()
            for (part in query.split('&')) {
                if (part.isEmpty()) continue
                val key = decode(part.substringBefore('='))
                val value = decode(part.substringAfter('=', ""))
                params.putIfAbsent(key, value) // first occurrence wins
            }
            fun need(key: String) = params[key]?.takeIf { it.isNotEmpty() }
                ?: throw Invalid(Problem.MALFORMED, "missing $key")

            if (need("v") != "1") throw Invalid(Problem.UNSUPPORTED_VERSION, "unsupported pairing code version ${params["v"]}")
            val id = need("id")
            val name = params["n"]?.takeIf { it.isNotBlank() } ?: id
            val hosts = need("h").split(',').map { it.trim().removePrefix("[").removeSuffix("]") }.filter { it.isNotEmpty() }
            if (hosts.isEmpty()) throw Invalid(Problem.MALFORMED, "no hosts")
            val port = need("p").toIntOrNull()?.takeIf { it in 1..65535 } ?: throw Invalid(Problem.MALFORMED, "bad port")
            val pin = need("pin")
            if (!Pins.isValid(pin)) throw Invalid(Problem.MALFORMED, "bad pin")
            val secret = need("s")
            if ((Base64Url.decode(secret)?.size ?: 0) < MIN_SECRET_BYTES) throw Invalid(Problem.MALFORMED, "bad secret")
            return PairingQr(id, name, hosts, port, pin, secret)
        }

        private fun decode(s: String): String = try {
            URLDecoder.decode(s, "UTF-8")
        } catch (e: IllegalArgumentException) {
            throw Invalid(Problem.MALFORMED, "bad percent-encoding")
        }

        /** The spec says 16; accept no less. */
        private const val MIN_SECRET_BYTES = 16
    }
}
