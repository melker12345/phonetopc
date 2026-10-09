package io.github.melker12345.phonetopc.ui

import androidx.annotation.StringRes
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.rounded.Autorenew
import androidx.compose.material.icons.rounded.BatteryFull
import androidx.compose.material.icons.rounded.LightMode
import androidx.compose.material.icons.rounded.ScreenRotation
import androidx.compose.material.icons.rounded.SensorOccupied
import androidx.compose.material.icons.rounded.Sensors
import androidx.compose.material.icons.rounded.Speed
import androidx.compose.material.icons.rounded.TouchApp
import androidx.compose.ui.graphics.vector.ImageVector
import io.github.melker12345.phonetopc.R
import io.github.melker12345.phonetopc.protocol.SensorCatalog

/** How a protocol source is presented in the UI. */
data class SensorUi(
    val source: String,
    @param:StringRes val label: Int,
    @param:StringRes val description: Int,
    val icon: ImageVector,
)

val SensorUiCatalog: List<SensorUi> = listOf(
    SensorUi(SensorCatalog.ACCELEROMETER, R.string.sensor_accelerometer, R.string.sensor_accelerometer_desc, Icons.Rounded.Speed),
    SensorUi(SensorCatalog.GYRO, R.string.sensor_gyro, R.string.sensor_gyro_desc, Icons.Rounded.Autorenew),
    SensorUi(SensorCatalog.GAME_ROTATION_VECTOR, R.string.sensor_rotation, R.string.sensor_rotation_desc, Icons.Rounded.ScreenRotation),
    SensorUi(SensorCatalog.PROXIMITY, R.string.sensor_proximity, R.string.sensor_proximity_desc, Icons.Rounded.SensorOccupied),
    SensorUi(SensorCatalog.LIGHT, R.string.sensor_light, R.string.sensor_light_desc, Icons.Rounded.LightMode),
    SensorUi(SensorCatalog.BATTERY, R.string.sensor_battery, R.string.sensor_battery_desc, Icons.Rounded.BatteryFull),
    SensorUi(SensorCatalog.UI, R.string.sensor_tile, R.string.sensor_tile_desc, Icons.Rounded.TouchApp),
)

fun sensorUi(source: String): SensorUi =
    SensorUiCatalog.firstOrNull { it.source == source }
        ?: SensorUi(source, R.string.sensor_unknown, R.string.sensor_unknown, Icons.Rounded.Sensors)
