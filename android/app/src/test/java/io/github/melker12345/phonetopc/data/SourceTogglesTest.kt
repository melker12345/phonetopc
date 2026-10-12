package io.github.melker12345.phonetopc.data

import io.github.melker12345.phonetopc.protocol.SensorCatalog
import org.junit.Assert.assertEquals
import org.junit.Test

class SourceTogglesTest {
    @Test fun locationIsOffUntilTurnedOn() {
        assertEquals(setOf(SensorCatalog.LOCATION), SourceToggles.disabled(emptySet(), emptySet()))
        assertEquals(setOf(SensorCatalog.LOCATION), AppSettings().disabledSources)
        assertEquals(emptySet<String>(), SourceToggles.disabled(emptySet(), setOf(SensorCatalog.LOCATION)))
    }

    @Test fun otherSourcesAreOnUntilTurnedOff() {
        assertEquals(
            setOf(SensorCatalog.GYRO, SensorCatalog.LOCATION),
            SourceToggles.disabled(setOf(SensorCatalog.GYRO), emptySet()),
        )
        // Only the matching set counts for each kind of source.
        assertEquals(
            setOf(SensorCatalog.GYRO),
            SourceToggles.disabled(setOf(SensorCatalog.GYRO, SensorCatalog.LOCATION), setOf(SensorCatalog.LOCATION, SensorCatalog.GYRO)),
        )
    }
}
