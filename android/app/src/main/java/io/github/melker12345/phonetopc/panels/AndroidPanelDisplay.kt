package io.github.melker12345.phonetopc.panels

import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import androidx.core.app.NotificationChannelCompat
import androidx.core.app.NotificationCompat
import androidx.core.app.NotificationManagerCompat
import io.github.melker12345.phonetopc.R
import io.github.melker12345.phonetopc.output.displayName
import io.github.melker12345.phonetopc.protocol.panels.PanelDisplay
import io.github.melker12345.phonetopc.protocol.panels.PanelSnapshot
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.asSharedFlow

/** A panel by PC and id: what the panel screen shows, across replacements of the panel. */
data class PanelKey(val pcKey: String, val panel: String)

val PanelSnapshot.key: PanelKey get() = PanelKey(pc.key, id)

/** What the panel screen tells the rest of the app while it is in front. */
object PanelScreen {
    /** The PC whose panel is on screen in the resumed panel screen, or `null`. */
    @Volatile var frontPcKey: String? = null

    private val _switchTo = MutableSharedFlow<PanelKey>(extraBufferCapacity = 8)

    /** Panels the resumed panel screen should switch to (PANELS.md §4.3). */
    val switchTo: SharedFlow<PanelKey> = _switchTo.asSharedFlow()

    internal fun requestSwitch(key: PanelKey) = _switchTo.tryEmit(key)
}

/**
 * PANELS.md §4.3, bringing a shown panel on screen: Android doesn't let an app in the background
 * open a screen, so a panel of the PC already in front is switched to directly, and anything
 * else becomes a "<PC> wants to show <title>" notification that opens it. A panel of another PC
 * on screen is never replaced; the new one is in the frame's switcher.
 */
class AndroidPanelDisplay(context: Context) : PanelDisplay {
    private val context = context.applicationContext

    override fun showRequested(panel: PanelSnapshot) {
        if (PanelScreen.frontPcKey == panel.pc.key) {
            PanelScreen.requestSwitch(panel.key)
        } else {
            PanelRequests.post(context, panel)
        }
    }
}

/** The "wants to show" notifications, one per panel, removed once it's seen or gone. */
object PanelRequests {
    const val CHANNEL_ID = "panel_requests"
    private const val NOTIFICATION_ID = 30
    private val posted = HashSet<PanelKey>()

    fun ensureChannel(context: Context) {
        NotificationManagerCompat.from(context).createNotificationChannel(
            NotificationChannelCompat.Builder(CHANNEL_ID, NotificationManagerCompat.IMPORTANCE_HIGH)
                .setName(context.getString(R.string.panel_request_channel_name))
                .setDescription(context.getString(R.string.panel_request_channel_description))
                .setShowBadge(false)
                .build(),
        )
    }

    fun post(context: Context, panel: PanelSnapshot) {
        ensureChannel(context)
        val open = PendingIntent.getActivity(
            context, 0,
            PanelActivity.intent(context, panel.key).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        val notification = NotificationCompat.Builder(context, CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_phonetopc)
            .setContentTitle(context.getString(R.string.panel_request_title, panel.pc.displayName(context), panel.title))
            .setContentText(context.getString(R.string.panel_request_text))
            .setContentIntent(open)
            .setAutoCancel(true)
            .setOnlyAlertOnce(true)
            .setPriority(NotificationCompat.PRIORITY_HIGH)
            .setVisibility(NotificationCompat.VISIBILITY_PRIVATE)
            .build()
        try {
            NotificationManagerCompat.from(context).notify(tag(panel.key), NOTIFICATION_ID, notification)
            synchronized(posted) { posted += panel.key }
        } catch (_: SecurityException) {
            // No POST_NOTIFICATIONS: the panel is still listed on the Devices screen.
        }
    }

    /** Removes the requests of panels that are now visible or no longer exist. */
    fun sync(context: Context, panels: List<PanelSnapshot>) {
        val stale = synchronized(posted) {
            val pending = panels.filter { !it.visible }.map { it.key }.toSet()
            posted.filter { it !in pending }.also { posted.removeAll(it.toSet()) }
        }
        val nm = NotificationManagerCompat.from(context)
        stale.forEach { nm.cancel(tag(it), NOTIFICATION_ID) }
    }

    private fun tag(key: PanelKey) = "panel-request:${key.pcKey}/${key.panel}"
}
