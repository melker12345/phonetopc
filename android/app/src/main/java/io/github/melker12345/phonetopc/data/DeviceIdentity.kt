package io.github.melker12345.phonetopc.data

import android.content.Context
import android.os.Build
import android.provider.Settings
import io.github.melker12345.phonetopc.protocol.DeviceInfo

object DeviceIdentity {
    /** User-visible device name (Settings > About phone), falling back to the model. */
    fun name(context: Context): String =
        Settings.Global.getString(context.contentResolver, Settings.Global.DEVICE_NAME)
            ?.takeIf { it.isNotBlank() }
            ?: Build.MODEL

    fun info(context: Context, id: String) = DeviceInfo(
        id = id,
        name = name(context),
        manufacturer = Build.MANUFACTURER,
        model = Build.MODEL,
        platform = "android",
        platformVersion = Build.VERSION.SDK_INT.toString(),
    )
}
