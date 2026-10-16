package io.github.melker12345.phonetopc.service

import android.app.Notification
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import androidx.core.app.NotificationChannelCompat
import androidx.core.app.NotificationCompat
import androidx.core.app.NotificationManagerCompat
import io.github.melker12345.phonetopc.MainActivity
import io.github.melker12345.phonetopc.R

internal object BridgeNotifications {
    const val CHANNEL_ID = "bridge"
    const val NOTIFICATION_ID = 1

    private const val AUTO_START_CHANNEL_ID = "auto_start"
    private const val TAP_TO_START_ID = 2

    fun ensureChannel(context: Context) {
        val channel = NotificationChannelCompat.Builder(CHANNEL_ID, NotificationManagerCompat.IMPORTANCE_LOW)
            .setName(context.getString(R.string.notification_channel_name))
            .setDescription(context.getString(R.string.notification_channel_description))
            .setShowBadge(false)
            .build()
        NotificationManagerCompat.from(context).createNotificationChannel(channel)
    }

    fun build(context: Context, status: BridgeStatus, address: String?): Notification {
        val clients = status.clients.size
        val title = when (status.state) {
            RunState.STARTING -> context.getString(R.string.notification_starting)
            RunState.PAUSED -> context.getString(R.string.notification_paused)
            else -> context.resources.getQuantityString(R.plurals.notification_clients, clients, clients)
        }
        val text = buildList {
            if (address != null && status.port != null) add("$address:${status.port}")
            if (status.state == RunState.RUNNING) {
                status.streaming.entries.sortedBy { it.key }.forEach { (src, d) ->
                    add(d.rateHz?.let { "$src ${it.toInt()} Hz" } ?: src)
                }
            }
        }.joinToString(" · ")

        val open = PendingIntent.getActivity(
            context, 0,
            Intent(context, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        val paused = status.state == RunState.PAUSED
        val pauseAction = NotificationCompat.Action(
            0,
            context.getString(if (paused) R.string.action_resume else R.string.action_pause),
            servicePending(context, if (paused) BridgeService.ACTION_RESUME else BridgeService.ACTION_PAUSE, 1),
        )
        val stopAction = NotificationCompat.Action(
            0, context.getString(R.string.action_stop), servicePending(context, BridgeService.ACTION_STOP, 2),
        )

        return NotificationCompat.Builder(context, CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_phonetopc)
            .setContentTitle(title)
            .setContentText(text.ifEmpty { null })
            .setContentIntent(open)
            .setOngoing(true)
            .setOnlyAlertOnce(true)
            .setSilent(true)
            .setShowWhen(false)
            .setCategory(NotificationCompat.CATEGORY_SERVICE)
            .setForegroundServiceBehavior(NotificationCompat.FOREGROUND_SERVICE_IMMEDIATE)
            .apply { if (status.state != RunState.STARTING) addAction(pauseAction) }
            .addAction(stopAction)
            .build()
    }

    /**
     * Plain (non-FGS) notification for when Android refused to start the service on its own.
     * Tapping opens the app, which then starts the server from the foreground.
     */
    fun postTapToStart(context: Context) {
        val nm = NotificationManagerCompat.from(context)
        // Without POST_NOTIFICATIONS (13+) there's nothing we can show.
        if (!nm.areNotificationsEnabled()) return
        nm.createNotificationChannel(
            NotificationChannelCompat.Builder(AUTO_START_CHANNEL_ID, NotificationManagerCompat.IMPORTANCE_DEFAULT)
                .setName(context.getString(R.string.auto_start_channel_name))
                .setDescription(context.getString(R.string.auto_start_channel_description))
                .setShowBadge(false)
                .build(),
        )
        val start = PendingIntent.getActivity(
            context, 3,
            Intent().setClassName(context, MainActivity.START_SERVER)
                .addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        val notification = NotificationCompat.Builder(context, AUTO_START_CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_phonetopc)
            .setContentTitle(context.getString(R.string.auto_start_title))
            .setContentText(context.getString(R.string.auto_start_text))
            .setContentIntent(start)
            .setAutoCancel(true)
            .setOnlyAlertOnce(true)
            .setCategory(NotificationCompat.CATEGORY_STATUS)
            .build()
        try {
            nm.notify(TAP_TO_START_ID, notification)
        } catch (_: SecurityException) {
            // POST_NOTIFICATIONS revoked between the check and the call.
        }
    }

    fun cancelTapToStart(context: Context) = NotificationManagerCompat.from(context).cancel(TAP_TO_START_ID)

    private fun servicePending(context: Context, action: String, requestCode: Int): PendingIntent =
        PendingIntent.getService(
            context, requestCode,
            Intent(context, BridgeService::class.java).setAction(action),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
}
