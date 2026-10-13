package io.github.melker12345.phonetopc.output

import io.github.melker12345.phonetopc.data.AppSettings
import io.github.melker12345.phonetopc.data.PairedPc
import io.github.melker12345.phonetopc.data.PairedPcRepository
import io.github.melker12345.phonetopc.data.SettingsRepository
import io.github.melker12345.phonetopc.protocol.panels.Feature
import io.github.melker12345.phonetopc.protocol.panels.OutputPermissions
import io.github.melker12345.phonetopc.protocol.panels.PcRef
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.filterNotNull
import kotlinx.coroutines.launch

/**
 * What the user allows each PC (PANELS.md §1), as one immutable value: paired PCs by id, and
 * the dev-mode PC. A PC that isn't paired (any more) gets nothing.
 */
data class OutputGrants(val pcs: Map<String, Set<Feature>> = emptyMap(), val dev: Set<Feature> = emptySet()) {
    fun allowed(pc: PcRef, feature: Feature): Boolean =
        if (pc.isDevMode) feature in dev else pcs[pc.id]?.contains(feature) == true

    companion object {
        fun of(pcs: List<PairedPc>, settings: AppSettings) = OutputGrants(
            pcs.associate { pc ->
                pc.id to buildSet {
                    if (pc.allowNotify) add(Feature.NOTIFY)
                    if (pc.allowPanels) add(Feature.PANELS)
                }
            },
            settings.devOutput,
        )
    }
}

/**
 * The server's synchronous view of [OutputGrants], kept up to date from the paired PCs and the
 * settings. Everything is denied until both have loaded. [onChanged] runs after every change,
 * so the services can remove what a PC may no longer show.
 */
class OutputAccess(
    scope: CoroutineScope,
    pairedPcs: PairedPcRepository,
    settings: SettingsRepository,
) : OutputPermissions {
    @Volatile private var grants = OutputGrants()

    @Volatile var onChanged: (() -> Unit)? = null

    init {
        scope.launch {
            combine(pairedPcs.loaded.filterNotNull(), settings.settings) { pcs, s -> OutputGrants.of(pcs, s) }
                .distinctUntilChanged()
                .collect {
                    grants = it
                    onChanged?.invoke()
                }
        }
    }

    override fun allowed(pc: PcRef, feature: Feature): Boolean = grants.allowed(pc, feature)
}
