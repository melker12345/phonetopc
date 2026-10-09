package io.github.melker12345.phonetopc.service

import io.github.melker12345.phonetopc.data.AppSettings
import io.github.melker12345.phonetopc.service.AutoStartTrigger.BOOT_COMPLETED
import io.github.melker12345.phonetopc.service.AutoStartTrigger.PACKAGE_REPLACED
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class AutoStartTest {
    private val running = AppSettings(desiredRunning = true)

    @Test
    fun defaultsNeverStart() {
        assertFalse(AutoStart.shouldStart(PACKAGE_REPLACED, AppSettings()))
        assertFalse(AutoStart.shouldStart(BOOT_COMPLETED, AppSettings()))
    }

    @Test
    fun updateRestartsWhenDesired() {
        assertTrue(AutoStart.shouldStart(PACKAGE_REPLACED, running))
        // Start on boot is irrelevant for updates.
        assertTrue(AutoStart.shouldStart(PACKAGE_REPLACED, running.copy(startOnBoot = true)))
    }

    @Test
    fun updateDoesNotStartWhenUserStoppedIt() {
        assertFalse(AutoStart.shouldStart(PACKAGE_REPLACED, running.copy(desiredRunning = false)))
    }

    @Test
    fun modeDoesNotMatter() {
        // Secure mode is the default and dev mode is the user's choice; both come back.
        for (dev in listOf(false, true)) {
            val s = running.copy(devMode = dev, startOnBoot = true)
            assertTrue(AutoStart.shouldStart(PACKAGE_REPLACED, s))
            assertTrue(AutoStart.shouldStart(BOOT_COMPLETED, s))
        }
    }

    @Test
    fun bootNeedsStartOnBoot() {
        assertFalse(AutoStart.shouldStart(BOOT_COMPLETED, running))
        assertTrue(AutoStart.shouldStart(BOOT_COMPLETED, running.copy(startOnBoot = true)))
    }

    @Test
    fun bootNeedsDesiredRunning() {
        val stopped = running.copy(desiredRunning = false, startOnBoot = true)
        assertFalse(AutoStart.shouldStart(BOOT_COMPLETED, stopped))
    }

    @Test
    fun exhaustiveTruthTable() {
        for (dev in listOf(false, true)) for (desired in listOf(false, true)) for (boot in listOf(false, true)) {
            val s = AppSettings(devMode = dev, desiredRunning = desired, startOnBoot = boot)
            assertEquals("update $s", desired, AutoStart.shouldStart(PACKAGE_REPLACED, s))
            assertEquals("boot $s", desired && boot, AutoStart.shouldStart(BOOT_COMPLETED, s))
        }
    }

    @Test
    fun mapsOnlyTheHandledActions() {
        assertEquals(PACKAGE_REPLACED, AutoStart.triggerFor("android.intent.action.MY_PACKAGE_REPLACED"))
        assertEquals(BOOT_COMPLETED, AutoStart.triggerFor("android.intent.action.BOOT_COMPLETED"))
        assertNull(AutoStart.triggerFor("android.intent.action.LOCKED_BOOT_COMPLETED"))
        assertNull(AutoStart.triggerFor("android.intent.action.PACKAGE_REPLACED"))
        assertNull(AutoStart.triggerFor(null))
    }
}
