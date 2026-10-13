package io.github.melker12345.phonetopc.data

import android.content.Context
import androidx.datastore.preferences.core.Preferences
import androidx.datastore.preferences.core.booleanPreferencesKey
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.intPreferencesKey
import androidx.datastore.preferences.core.stringPreferencesKey
import androidx.datastore.preferences.core.stringSetPreferencesKey
import androidx.datastore.preferences.preferencesDataStore
import io.github.melker12345.phonetopc.protocol.SensorCatalog
import io.github.melker12345.phonetopc.protocol.panels.Feature
import io.github.melker12345.phonetopc.server.DEFAULT_PORT
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.map
import java.security.SecureRandom

data class AppSettings(
    /**
     * "Developer mode (unencrypted)": serve plain `ws://` without pairing (draft 1). Off means
     * secure mode: `wss://` and a paired PC's token (SECURITY.md §3).
     */
    val devMode: Boolean = false,
    val port: Int = DEFAULT_PORT,
    /**
     * Sources the user has turned off, or never turned on if they are off by default
     * ([SourceToggles.OFF_BY_DEFAULT]); they are reported as `available: false`.
     */
    val disabledSources: Set<String> = SourceToggles.OFF_BY_DEFAULT,
    val onboardingDone: Boolean = false,
    /**
     * The user wants the server running: set when they start it, cleared when they stop it
     * (UI or notification Stop). Pause keeps it set. Used to bring
     * the service back after an app update or a reboot.
     */
    val desiredRunning: Boolean = false,
    /** "Start on boot": also bring the server back after a reboot, not just after an update. */
    val startOnBoot: Boolean = false,
    /**
     * PANELS.md §1: what the dev-mode PC (every client in developer mode) may do: notifications
     * and panels, each off until turned on.
     */
    val devOutput: Set<Feature> = emptySet(),
)

/** The per-sensor allow toggles of Settings › Sensors. */
object SourceToggles {
    /** Privacy-sensitive sources the user has to turn on first. */
    val OFF_BY_DEFAULT: Set<String> = setOf(SensorCatalog.LOCATION)

    /**
     * The effective set of turned-off sources, from the stored sets: [turnedOff] holds sources
     * the user turned off, [turnedOn] off-by-default sources the user turned on.
     */
    fun disabled(turnedOff: Set<String>, turnedOn: Set<String>): Set<String> =
        (turnedOff - OFF_BY_DEFAULT) + (OFF_BY_DEFAULT - turnedOn)
}

private val Context.dataStore by preferencesDataStore(name = "settings")

class SettingsRepository(context: Context) {
    private val store = context.applicationContext.dataStore

    val settings: Flow<AppSettings> = store.data.map { it.toSettings() }

    suspend fun current(): AppSettings = settings.first()

    suspend fun setDevMode(enabled: Boolean) = store.edit { it[DEV_MODE] = enabled }

    suspend fun setPort(port: Int) {
        require(port in MIN_PORT..MAX_PORT) { "port out of range" }
        store.edit { it[PORT] = port }
    }

    suspend fun setSourceEnabled(source: String, enabled: Boolean) = store.edit {
        // Off-by-default sources remember being turned on; the others being turned off.
        if (source in SourceToggles.OFF_BY_DEFAULT) {
            val on = it[ENABLED_SOURCES].orEmpty()
            it[ENABLED_SOURCES] = if (enabled) on + source else on - source
        } else {
            val off = it[DISABLED_SOURCES].orEmpty()
            it[DISABLED_SOURCES] = if (enabled) off - source else off + source
        }
    }

    suspend fun setOnboardingDone(done: Boolean) = store.edit { it[ONBOARDING_DONE] = done }

    suspend fun setDesiredRunning(desired: Boolean) = store.edit { it[DESIRED_RUNNING] = desired }

    suspend fun setStartOnBoot(enabled: Boolean) = store.edit { it[START_ON_BOOT] = enabled }

    suspend fun setDevOutputAllowed(feature: Feature, allowed: Boolean) = store.edit {
        val on = it[DEV_OUTPUT].orEmpty()
        it[DEV_OUTPUT] = if (allowed) on + feature.wireName else on - feature.wireName
    }

    /** Random id generated on first run and kept for the lifetime of the install (spec §4.1). */
    suspend fun deviceId(): String {
        store.data.first()[DEVICE_ID]?.let { return it }
        var id = ""
        store.edit { prefs ->
            id = prefs[DEVICE_ID] ?: newDeviceId().also { prefs[DEVICE_ID] = it }
        }
        return id
    }

    private fun Preferences.toSettings() = AppSettings(
        devMode = this[DEV_MODE] ?: false,
        port = this[PORT] ?: DEFAULT_PORT,
        disabledSources = SourceToggles.disabled(this[DISABLED_SOURCES].orEmpty(), this[ENABLED_SOURCES].orEmpty()),
        onboardingDone = this[ONBOARDING_DONE] ?: false,
        desiredRunning = this[DESIRED_RUNNING] ?: false,
        startOnBoot = this[START_ON_BOOT] ?: false,
        devOutput = Feature.entries.filter { it.wireName in this[DEV_OUTPUT].orEmpty() }.toSet(),
    )

    companion object {
        const val MIN_PORT = 1024
        const val MAX_PORT = 65535

        private val DEV_MODE = booleanPreferencesKey("dev_mode")
        private val PORT = intPreferencesKey("port")
        private val DISABLED_SOURCES = stringSetPreferencesKey("disabled_sources")
        private val ENABLED_SOURCES = stringSetPreferencesKey("enabled_sources")
        private val ONBOARDING_DONE = booleanPreferencesKey("onboarding_done")
        private val DESIRED_RUNNING = booleanPreferencesKey("desired_running")
        private val START_ON_BOOT = booleanPreferencesKey("start_on_boot")
        private val DEVICE_ID = stringPreferencesKey("device_id")
        private val DEV_OUTPUT = stringSetPreferencesKey("dev_output")

        private fun newDeviceId(): String {
            val bytes = ByteArray(8).also { SecureRandom().nextBytes(it) }
            return bytes.joinToString("") { "%02x".format(it) }
        }
    }
}
