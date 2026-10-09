package io.github.melker12345.phonetopc.data

import android.content.Context
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.stringPreferencesKey
import androidx.datastore.preferences.preferencesDataStore
import io.github.melker12345.phonetopc.pairing.NewPairing
import io.github.melker12345.phonetopc.protocol.PcInfo
import io.github.melker12345.phonetopc.protocol.ProtocolJson
import io.github.melker12345.phonetopc.protocol.security.ConstantTime
import io.github.melker12345.phonetopc.protocol.security.TokenHash
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.filterNotNull
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.launch
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.buildJsonArray
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.longOrNull
import kotlinx.serialization.json.put

/**
 * A PC this phone has paired with (SECURITY.md §4). Only a hash of its token is kept
 * (§2 allows that), so the settings file doesn't contain anything that logs in.
 */
data class PairedPc(
    val id: String,
    val name: String,
    val pin: String,
    val tokenHash: String,
    val pairedAt: Long,
    val lastConnected: Long? = null,
) {
    val info: PcInfo get() = PcInfo(id, name)
}

/** Pure list operations and the storage format, kept apart so they can be unit-tested. */
object PairedPcList {
    /** §4.4: re-pairing a PC we know (same id) replaces its token. */
    fun upsert(list: List<PairedPc>, pairing: NewPairing, now: Long): List<PairedPc> {
        val entry = PairedPc(pairing.pcId, pairing.pcName, pairing.pin, TokenHash.of(pairing.token), now)
        return list.filterNot { it.id == pairing.pcId } + entry
    }

    fun remove(list: List<PairedPc>, id: String) = list.filterNot { it.id == id }

    fun touch(list: List<PairedPc>, id: String, now: Long) =
        list.map { if (it.id == id) it.copy(lastConnected = now) else it }

    /**
     * The PC a token belongs to. Hashes the token once and compares it with every stored hash
     * in constant time, without stopping early.
     */
    fun authenticate(list: List<PairedPc>, token: String): PairedPc? {
        val hash = TokenHash.of(token)
        var found: PairedPc? = null
        for (pc in list) if (ConstantTime.equals(hash, pc.tokenHash) && found == null) found = pc
        return found
    }

    fun encode(list: List<PairedPc>): String = buildJsonArray {
        for (pc in list) add(
            buildJsonObject {
                put("id", pc.id)
                put("name", pc.name)
                put("pin", pc.pin)
                put("token_sha256", pc.tokenHash)
                put("paired_at", pc.pairedAt)
                pc.lastConnected?.let { put("last_connected", it) }
            },
        )
    }.toString()

    fun decode(text: String?): List<PairedPc> {
        if (text.isNullOrBlank()) return emptyList()
        val array = runCatching { ProtocolJson.parseToJsonElement(text) as? JsonArray }.getOrNull() ?: return emptyList()
        return array.mapNotNull { e ->
            val o = e as? JsonObject ?: return@mapNotNull null
            fun s(k: String) = (o[k] as? JsonPrimitive)?.takeIf { it.isString }?.content
            fun l(k: String) = (o[k] as? JsonPrimitive)?.longOrNull
            PairedPc(
                id = s("id") ?: return@mapNotNull null,
                name = s("name") ?: "",
                pin = s("pin") ?: return@mapNotNull null,
                tokenHash = s("token_sha256") ?: return@mapNotNull null,
                pairedAt = l("paired_at") ?: 0,
                lastConnected = l("last_connected"),
            )
        }
    }
}

private val Context.pairingStore by preferencesDataStore(name = "paired_pcs")

/**
 * Paired PCs, persisted in DataStore (app-private, excluded from backups). A copy is kept in
 * memory for the server's synchronous token checks.
 */
class PairedPcRepository(context: Context, scope: CoroutineScope) {
    private val store = context.applicationContext.pairingStore

    val pcs: Flow<List<PairedPc>> = store.data.map { PairedPcList.decode(it[KEY]) }

    private val _loaded = MutableStateFlow<List<PairedPc>?>(null)

    /** `null` until loaded. */
    val loaded: StateFlow<List<PairedPc>?> = _loaded.asStateFlow()

    init {
        scope.launch { pcs.collect { _loaded.value = it } }
    }

    /** The current list, waiting for the first load. */
    suspend fun current(): List<PairedPc> = _loaded.filterNotNull().first()

    /** For `hello`: never blocks; empty until loaded, which [current] lets callers wait for. */
    fun authenticate(token: String): PairedPc? = PairedPcList.authenticate(_loaded.value.orEmpty(), token)

    suspend fun add(pairing: NewPairing, now: Long = System.currentTimeMillis()) = update { PairedPcList.upsert(it, pairing, now) }

    suspend fun remove(id: String) = update { PairedPcList.remove(it, id) }

    suspend fun touch(id: String, now: Long = System.currentTimeMillis()) = update { PairedPcList.touch(it, id, now) }

    private suspend fun update(transform: (List<PairedPc>) -> List<PairedPc>) {
        val saved = store.edit { prefs ->
            prefs[KEY] = PairedPcList.encode(transform(PairedPcList.decode(prefs[KEY])))
        }
        // Apply right away, so a revoked token stops working before the flow catches up.
        _loaded.value = PairedPcList.decode(saved[KEY])
    }

    private companion object {
        val KEY = stringPreferencesKey("pcs")
    }
}
