package io.github.melker12345.phonetopc.output

import io.github.melker12345.phonetopc.data.AppSettings
import io.github.melker12345.phonetopc.data.PairedPc
import io.github.melker12345.phonetopc.protocol.panels.Feature
import io.github.melker12345.phonetopc.protocol.panels.PcRef
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class OutputGrantsTest {
    private fun pc(id: String, notify: Boolean = false, panels: Boolean = false) =
        PairedPc(id, "pc $id", "sha256/x", "h", 0, allowNotify = notify, allowPanels = panels)

    @Test fun offByDefault() {
        val grants = OutputGrants.of(listOf(pc("a")), AppSettings())
        for (f in Feature.entries) {
            assertFalse(grants.allowed(PcRef("a", "pc a"), f))
            assertFalse(grants.allowed(PcRef.DEV, f))
        }
    }

    @Test fun perPcAndPerFeature() {
        val grants = OutputGrants.of(listOf(pc("a", notify = true), pc("b", panels = true)), AppSettings())
        assertTrue(grants.allowed(PcRef("a", "x"), Feature.NOTIFY))
        assertFalse(grants.allowed(PcRef("a", "x"), Feature.PANELS))
        assertFalse(grants.allowed(PcRef("b", "x"), Feature.NOTIFY))
        assertTrue(grants.allowed(PcRef("b", "x"), Feature.PANELS))
    }

    @Test fun unpairedPcGetsNothing() {
        val grants = OutputGrants.of(listOf(pc("a", notify = true, panels = true)), AppSettings())
        assertFalse(grants.allowed(PcRef("gone", "x"), Feature.NOTIFY))
        assertFalse(grants.allowed(PcRef("gone", "x"), Feature.PANELS))
    }

    @Test fun devModePcHasItsOwnSettings() {
        val grants = OutputGrants.of(listOf(pc("a", notify = true, panels = true)), AppSettings(devOutput = setOf(Feature.PANELS)))
        assertFalse(grants.allowed(PcRef.DEV, Feature.NOTIFY))
        assertTrue(grants.allowed(PcRef.DEV, Feature.PANELS))
    }
}
