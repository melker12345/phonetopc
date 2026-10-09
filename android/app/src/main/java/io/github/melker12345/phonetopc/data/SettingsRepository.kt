package io.github.melker12345.phonetopc.data

import android.content.Context
import androidx.datastore.preferences.core.Preferences
import androidx.datastore.preferences.core.booleanPreferencesKey
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.intPreferencesKey
import androidx.datastore.preferences.core.stringPreferencesKey
import androidx.datastore.preferences.core.stringSetPreferencesKey
import androidx.datastore.preferences.preferencesDataStore
import io.github.melker12345.phonetopc.server.DEFAULT_PORT
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.map
import java.security.SecureRandom

data class AppSettings(
    /** "Developer mode (unencrypted)": the only way the plaintext draft-1 server may run. */
    val devMode: Boolean = false,
    val port: Int = DEFAULT_PORT,
    /** Sources the user has turned off; they are reported as `available: false`. */
    val disabledSources: Set<String> = emptySet(),
    val onboardingDone: Boolean = false,
    /**
     * The user wants the server running: set when they start it, cleared when they stop it
     * (UI, notification Stop, or turning developer mode off). Pause keeps it set. Used to bring
     * the service back after an app update or a reboot.
     */
    val desiredRunning: Boolean = false,
    /** "Start on boot": also bring the server back after a reboot, not just after an update. */
    val startOnBoot: Boolean = false,
)

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
        val current = it[DISABLED_SOURCES].orEmpty()
        it[DISABLED_SOURCES] = if (enabled) current - source else current + source
    }

    suspend fun setOnboardingDone(done: Boolean) = store.edit { it[ONBOARDING_DONE] = done }

    suspend fun setDesiredRunning(desired: Boolean) = store.edit { it[DESIRED_RUNNING] = desired }

    suspend fun setStartOnBoot(enabled: Boolean) = store.edit { it[START_ON_BOOT] = enabled }

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
        disabledSources = this[DISABLED_SOURCES].orEmpty(),
        onboardingDone = this[ONBOARDING_DONE] ?: false,
        desiredRunning = this[DESIRED_RUNNING] ?: false,
        startOnBoot = this[START_ON_BOOT] ?: false,
    )

    companion object {
        const val MIN_PORT = 1024
        const val MAX_PORT = 65535

        private val DEV_MODE = booleanPreferencesKey("dev_mode")
        private val PORT = intPreferencesKey("port")
        private val DISABLED_SOURCES = stringSetPreferencesKey("disabled_sources")
        private val ONBOARDING_DONE = booleanPreferencesKey("onboarding_done")
        private val DESIRED_RUNNING = booleanPreferencesKey("desired_running")
        private val START_ON_BOOT = booleanPreferencesKey("start_on_boot")
        private val DEVICE_ID = stringPreferencesKey("device_id")

        private fun newDeviceId(): String {
            val bytes = ByteArray(8).also { SecureRandom().nextBytes(it) }
            return bytes.joinToString("") { "%02x".format(it) }
        }
    }
}
