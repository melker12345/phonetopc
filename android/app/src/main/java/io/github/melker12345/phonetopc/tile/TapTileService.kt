package io.github.melker12345.phonetopc.tile

import android.service.quicksettings.Tile
import android.service.quicksettings.TileService
import io.github.melker12345.phonetopc.appGraph
import io.github.melker12345.phonetopc.service.RunState

/** Quick Settings tile; every tap becomes a `ui`/`tile_tap` event. Shown active while serving. */
class TapTileService : TileService() {
    override fun onStartListening() {
        super.onStartListening()
        val tile = qsTile ?: return
        tile.state = if (appGraph.bridge.status.value.state == RunState.RUNNING) Tile.STATE_ACTIVE else Tile.STATE_INACTIVE
        tile.updateTile()
    }

    override fun onClick() {
        super.onClick()
        appGraph.sensorSource.emitTileTap()
    }
}
