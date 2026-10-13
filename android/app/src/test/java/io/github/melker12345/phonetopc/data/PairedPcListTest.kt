package io.github.melker12345.phonetopc.data

import io.github.melker12345.phonetopc.pairing.NewPairing
import io.github.melker12345.phonetopc.protocol.panels.Feature
import io.github.melker12345.phonetopc.protocol.security.Secrets
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class PairedPcListTest {
    private val pin = "sha256/89Nebgssmdcpo6zU3xFLFXCRqV4YpVYbvJbyq99cMGA"
    private val tokenA = Secrets.newToken()
    private val tokenB = Secrets.newToken()
    private val a = NewPairing("pc-a", "arch-desktop", pin, tokenA)
    private val b = NewPairing("pc-b", "laptop", pin, tokenB)

    @Test fun storesOnlyAHashOfTheToken() {
        val list = PairedPcList.upsert(emptyList(), a, now = 1)
        val stored = list.single()
        assertFalse(stored.tokenHash == tokenA)
        assertFalse(PairedPcList.encode(list).contains(tokenA))
        assertEquals("pc-a", PairedPcList.authenticate(list, tokenA)?.id)
    }

    @Test fun authenticatesTheRightPc() {
        val list = PairedPcList.upsert(PairedPcList.upsert(emptyList(), a, 1), b, 2)
        assertEquals("pc-a", PairedPcList.authenticate(list, tokenA)?.id)
        assertEquals("pc-b", PairedPcList.authenticate(list, tokenB)?.id)
        assertNull(PairedPcList.authenticate(list, Secrets.newToken()))
        assertNull(PairedPcList.authenticate(list, ""))
    }

    @Test fun repairingReplacesTheToken() {
        val newToken = Secrets.newToken()
        var list = PairedPcList.upsert(emptyList(), a, 1)
        list = PairedPcList.upsert(list, a.copy(pcName = "renamed", token = newToken), 5)
        assertEquals(1, list.size)
        assertEquals("renamed", list.single().name)
        assertEquals(5L, list.single().pairedAt)
        assertNull(PairedPcList.authenticate(list, tokenA))
        assertEquals("pc-a", PairedPcList.authenticate(list, newToken)?.id)
    }

    @Test fun revokeRemovesAccess() {
        val list = PairedPcList.remove(PairedPcList.upsert(PairedPcList.upsert(emptyList(), a, 1), b, 2), "pc-a")
        assertNull(PairedPcList.authenticate(list, tokenA))
        assertEquals("pc-b", PairedPcList.authenticate(list, tokenB)?.id)
    }

    @Test fun encodeDecodeRoundTripAndTouch() {
        var list = PairedPcList.upsert(PairedPcList.upsert(emptyList(), a, 1), b, 2)
        list = PairedPcList.touch(list, "pc-b", 99)
        val decoded = PairedPcList.decode(PairedPcList.encode(list))
        assertEquals(list, decoded)
        assertEquals(99L, decoded.first { it.id == "pc-b" }.lastConnected)
        assertNull(decoded.first { it.id == "pc-a" }.lastConnected)
    }

    @Test fun outputPermissionsRoundTripAndResetOnRepairing() {
        var list = PairedPcList.upsert(PairedPcList.upsert(emptyList(), a, 1), b, 2)
        list = PairedPcList.setAllowed(list, "pc-a", Feature.NOTIFY, true)
        list = PairedPcList.setAllowed(list, "pc-a", Feature.PANELS, true)
        val decoded = PairedPcList.decode(PairedPcList.encode(list))
        assertEquals(list, decoded)
        assertTrue(decoded.first { it.id == "pc-a" }.allowNotify)
        assertTrue(decoded.first { it.id == "pc-a" }.allowPanels)
        assertFalse(decoded.first { it.id == "pc-b" }.allowPanels)
        // Whoever pairs as pc-a again has to be allowed again.
        val repaired = PairedPcList.upsert(list, a.copy(token = Secrets.newToken()), 3).first { it.id == "pc-a" }
        assertFalse(repaired.allowNotify)
        assertFalse(repaired.allowPanels)
    }

    @Test fun decodeToleratesGarbage() {
        assertEquals(emptyList<PairedPc>(), PairedPcList.decode(null))
        assertEquals(emptyList<PairedPc>(), PairedPcList.decode("not json"))
        assertEquals(emptyList<PairedPc>(), PairedPcList.decode("""[{"id":"x"}, 5]"""))
    }
}
