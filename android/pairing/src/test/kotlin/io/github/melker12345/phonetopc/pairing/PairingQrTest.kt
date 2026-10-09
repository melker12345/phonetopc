package io.github.melker12345.phonetopc.pairing

import io.github.melker12345.phonetopc.pairing.PairingQr.Companion.Problem
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Test

class PairingQrTest {
    private val pin = "sha256/89Nebgssmdcpo6zU3xFLFXCRqV4YpVYbvJbyq99cMGA"
    private val secret = "AAECAwQFBgcICQoLDA0ODw"

    @Test fun parsesTheSpecExample() {
        val qr = PairingQr.parse(
            "phonetopc://pair?v=1&id=9d1c0a&n=arch%20desktop&h=192.168.1.10,fd00::10&p=40123&pin=$pin&s=$secret",
        )
        assertEquals("9d1c0a", qr.pcId)
        assertEquals("arch desktop", qr.pcName)
        assertEquals(listOf("192.168.1.10", "fd00::10"), qr.hosts)
        assertEquals(40123, qr.port)
        assertEquals(pin, qr.pin)
        assertEquals(secret, qr.secret)
    }

    @Test fun acceptsFormEncodingAndEncodedSeparators() {
        // Go's url.Values encodes ',' as %2C, ':' as %3A and spaces as '+'.
        val qr = PairingQr.parse(
            "phonetopc://pair?v=1&id=x&n=Melker%27s+PC&h=192.168.1.10%2Cfd00%3A%3A10&p=1&pin=${pin.replace("/", "%2F")}&s=$secret&extra=1",
        )
        assertEquals("Melker's PC", qr.pcName)
        assertEquals(listOf("192.168.1.10", "fd00::10"), qr.hosts)
        assertEquals(pin, qr.pin)
    }

    @Test fun followsTheDraft3EncodingRules() {
        // SECURITY.md §4.2: n is UTF-8 percent-encoded, commas in h literal, IPv6 without brackets.
        val qr = PairingQr.parse(
            "phonetopc://pair?v=1&id=x&n=Melkers%20B%C3%A4rbar%20%26%20Co&h=192.168.1.10,fd00::10,10.0.0.5&p=8766&pin=$pin&s=$secret",
        )
        assertEquals("Melkers Bärbar & Co", qr.pcName)
        assertEquals(listOf("192.168.1.10", "fd00::10", "10.0.0.5"), qr.hosts)
        assertEquals(8766, qr.port)
        // Consumers also accept '+', %2C and bracketed IPv6, also percent-encoded and mixed.
        val lenient = PairingQr.parse(
            "phonetopc://pair?v=1&id=x&n=arch+desktop&h=%5Bfd00%3A%3A10%5D%2C192.168.1.10,[fd00::11]&p=8766&pin=$pin&s=$secret",
        )
        assertEquals("arch desktop", lenient.pcName)
        assertEquals(listOf("fd00::10", "192.168.1.10", "fd00::11"), lenient.hosts)
    }

    @Test fun bracketedIpv6IsUnwrapped() {
        val qr = PairingQr.parse("phonetopc://pair?v=1&id=x&h=[fd00::1]&p=1&pin=$pin&s=$secret")
        assertEquals(listOf("fd00::1"), qr.hosts)
        assertEquals("x", qr.pcName) // n missing: fall back to the id
    }

    @Test fun secretIsNotInToString() {
        val qr = PairingQr.parse("phonetopc://pair?v=1&id=x&h=1.2.3.4&p=1&pin=$pin&s=$secret")
        assertFalse(qr.toString().contains(secret))
    }

    @Test fun rejectsBadCodes() {
        fun expect(problem: Problem, text: String) {
            try {
                PairingQr.parse(text)
                fail("accepted $text")
            } catch (e: PairingQr.Companion.Invalid) {
                assertEquals(text, problem, e.problem)
            }
        }
        val ok = "id=x&h=1.2.3.4&p=1&pin=$pin&s=$secret"
        expect(Problem.NOT_PHONETOPC, "https://example.com")
        expect(Problem.UNSUPPORTED_VERSION, "phonetopc://pair?v=2&$ok")
        expect(Problem.MALFORMED, "phonetopc://pair?$ok")
        expect(Problem.MALFORMED, "phonetopc://pair?v=1&id=x&h=&p=1&pin=$pin&s=$secret")
        expect(Problem.MALFORMED, "phonetopc://pair?v=1&id=x&h=1.2.3.4&p=0&pin=$pin&s=$secret")
        expect(Problem.MALFORMED, "phonetopc://pair?v=1&id=x&h=1.2.3.4&p=1&pin=sha256/short&s=$secret")
        expect(Problem.MALFORMED, "phonetopc://pair?v=1&id=x&h=1.2.3.4&p=1&pin=$pin&s=AAEC")
        expect(Problem.MALFORMED, "phonetopc://pair?v=1&id=x&h=1.2.3.4&p=1&pin=$pin&s=$secret%zz")
    }

    @Test fun recognisesOurScheme() {
        assertTrue(PairingQr.looksLikePairingUri("  phonetopc://pair?v=1"))
        assertFalse(PairingQr.looksLikePairingUri("phonetopc://other"))
    }
}
