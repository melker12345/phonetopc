package io.github.melker12345.phonetopc

import android.app.Application
import android.content.Context
import io.github.melker12345.phonetopc.data.SettingsRepository
import io.github.melker12345.phonetopc.sensors.AndroidSensorSource

/** Process-wide singletons. Small enough that a DI framework isn't worth it. */
class AppGraph(context: Context) {
    val settings = SettingsRepository(context)
    val sensorSource = AndroidSensorSource(context)
}

class PhonetopcApp : Application() {
    lateinit var graph: AppGraph
        private set

    override fun onCreate() {
        super.onCreate()
        graph = AppGraph(this)
    }
}

val Context.appGraph: AppGraph get() = (applicationContext as PhonetopcApp).graph
