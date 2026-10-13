package io.github.melker12345.phonetopc.output

import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.net.Uri
import androidx.core.app.NotificationChannelCompat
import androidx.core.app.NotificationCompat
import androidx.core.app.NotificationManagerCompat
import io.github.melker12345.phonetopc.R
import io.github.melker12345.phonetopc.appGraph
import io.github.melker12345.phonetopc.protocol.panels.PcNotification
import io.github.melker12345.phonetopc.protocol.panels.PcNotifier
import io.github.melker12345.phonetopc.protocol.panels.PcRef

/** The name the phone shows for [this] PC: from pairing, or "Unverified PC (dev mode)" (PANELS.md §1). */
fun PcRef.displayName(context: Context): String = if (isDevMode) context.getString(R.string.panel_dev_pc_name) else name

/**
 * PANELS.md §3: a PC's notifications on the channel "PC notifications" (and a heads-up twin for
 * `priority: high`), with the PC's pairing name as the source and up to three actions. Each
 * notification is tagged with its PC and id, so ids of different PCs never collide.
 */
class AndroidPcNotifier(context: Context) : PcNotifier {
    private val context = context.applicationContext

    override fun post(notification: PcNotification) {
        ensureChannels(context)
        val pc = notification.pc
        val id = notification.id
        val builder = NotificationCompat.Builder(context, if (notification.highPriority) CHANNEL_HIGH else CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_phonetopc)
            .setContentTitle(notification.title)
            .setSubText(pc.displayName(context))
            .setContentIntent(PcNotificationReceiver.pending(context, PcNotificationReceiver.ACTION_TAP, pc, id))
            .setDeleteIntent(PcNotificationReceiver.pending(context, PcNotificationReceiver.ACTION_DISMISS, pc, id))
            .setAutoCancel(true)
            .setPriority(if (notification.highPriority) NotificationCompat.PRIORITY_HIGH else NotificationCompat.PRIORITY_DEFAULT)
            .setVisibility(NotificationCompat.VISIBILITY_PRIVATE)
            .setPublicVersion(
                NotificationCompat.Builder(context, CHANNEL_ID)
                    .setSmallIcon(R.drawable.ic_phonetopc)
                    .setContentTitle(context.getString(R.string.pc_notification_public_title))
                    .build(),
            )
        if (notification.body.isNotEmpty()) {
            builder.setContentText(notification.body).setStyle(NotificationCompat.BigTextStyle().bigText(notification.body))
        }
        for (action in notification.actions) {
            builder.addAction(0, action.label, PcNotificationReceiver.pending(context, PcNotificationReceiver.ACTION_ACTION, pc, id, action.id))
        }
        try {
            NotificationManagerCompat.from(context).notify(tag(pc, id), NOTIFICATION_ID, builder.build())
        } catch (_: SecurityException) {
            // No POST_NOTIFICATIONS (13+): nobody sees it, as with a blocked channel.
        }
    }

    override fun cancel(pc: PcRef, id: String) = NotificationManagerCompat.from(context).cancel(tag(pc, id), NOTIFICATION_ID)

    companion object {
        const val CHANNEL_ID = "pc_notifications"
        const val CHANNEL_HIGH = "pc_notifications_high"
        private const val NOTIFICATION_ID = 20

        private fun tag(pc: PcRef, id: String) = "pc-notification:${pc.key}/$id"

        fun ensureChannels(context: Context) {
            val nm = NotificationManagerCompat.from(context)
            nm.createNotificationChannel(
                NotificationChannelCompat.Builder(CHANNEL_ID, NotificationManagerCompat.IMPORTANCE_DEFAULT)
                    .setName(context.getString(R.string.pc_notification_channel_name))
                    .setDescription(context.getString(R.string.pc_notification_channel_description))
                    .build(),
            )
            nm.createNotificationChannel(
                NotificationChannelCompat.Builder(CHANNEL_HIGH, NotificationManagerCompat.IMPORTANCE_HIGH)
                    .setName(context.getString(R.string.pc_notification_high_channel_name))
                    .setDescription(context.getString(R.string.pc_notification_high_channel_description))
                    .build(),
            )
        }
    }
}

/** The user's answer to a PC notification: an action, a tap, or swiping it away (PANELS.md §3.2). */
class PcNotificationReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        val id = intent.getStringExtra(EXTRA_ID) ?: return
        val pc = PcRef(intent.getStringExtra(EXTRA_PC_ID), intent.getStringExtra(EXTRA_PC_NAME).orEmpty())
        val notifications = context.appGraph.notifications
        when (intent.action) {
            ACTION_ACTION -> notifications.onAction(pc, id, intent.getStringExtra(EXTRA_ACTION) ?: return)
            ACTION_TAP -> notifications.onTap(pc, id)
            ACTION_DISMISS -> notifications.onDismiss(pc, id)
        }
    }

    companion object {
        const val ACTION_ACTION = "io.github.melker12345.phonetopc.action.PC_NOTIFICATION_ACTION"
        const val ACTION_TAP = "io.github.melker12345.phonetopc.action.PC_NOTIFICATION_TAP"
        const val ACTION_DISMISS = "io.github.melker12345.phonetopc.action.PC_NOTIFICATION_DISMISS"
        private const val EXTRA_PC_ID = "pc_id"
        private const val EXTRA_PC_NAME = "pc_name"
        private const val EXTRA_ID = "id"
        private const val EXTRA_ACTION = "action"

        fun pending(context: Context, action: String, pc: PcRef, id: String, actionId: String? = null): PendingIntent {
            val intent = Intent(context, PcNotificationReceiver::class.java)
                .setAction(action)
                // Distinct data per PC, notification and action keeps each PendingIntent separate.
                .setData(
                    Uri.Builder().scheme("phonetopc").authority("pc-notification")
                        .appendPath(pc.key).appendPath(id).appendPath(actionId ?: "").build(),
                )
                .putExtra(EXTRA_PC_ID, pc.id)
                .putExtra(EXTRA_PC_NAME, pc.name)
                .putExtra(EXTRA_ID, id)
                .putExtra(EXTRA_ACTION, actionId)
            return PendingIntent.getBroadcast(context, 0, intent, PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
        }
    }
}
