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
    private val running = AppSettings(devMode = true, desiredRunning = true)

    @Test
    fun defaultsNeverStart() {
        assertFalse(AutoStart.shouldStart(PACKAGE_REPLACED, AppSettings()))
        assertFalse(AutoStart.shouldStart(BOOT_COMPLETED, AppSettings()))
    }

    @Test
    fun updateRestartsWhenDesiredAndDevMode() {
        assertTrue(AutoStart.shouldStart(PACKAGE_REPLACED, running))
        // Start on boot is irrelevant for updates.
        assertTrue(AutoStart.shouldStart(PACKAGE_REPLACED, running.copy(startOnBoot = true)))
    }

    @Test
    fun updateDoesNotStartWhenUserStoppedIt() {
        assertFalse(AutoStart.shouldStart(PACKAGE_REPLACED, running.copy(desiredRunning = false)))
    }

    @Test
    fun nothingStartsWithDevModeOff() {
        val off = running.copy(devMode = false, startOnBoot = true)
        assertFalse(AutoStart.shouldStart(PACKAGE_REPLACED, off))
        assertFalse(AutoStart.shouldStart(BOOT_COMPLETED, off))
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
            assertEquals("update $s", dev && desired, AutoStart.shouldStart(PACKAGE_REPLACED, s))
            assertEquals("boot $s", dev && desired && boot, AutoStart.shouldStart(BOOT_COMPLETED, s))
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
