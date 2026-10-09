package io.github.melker12345.phonetopc.tile

import android.service.quicksettings.TileService
import io.github.melker12345.phonetopc.appGraph

/** Quick Settings tile; every tap becomes a `ui`/`tile_tap` event. */
class TapTileService : TileService() {
    override fun onClick() {
        super.onClick()
        appGraph.sensorSource.emitTileTap()
    }
}
