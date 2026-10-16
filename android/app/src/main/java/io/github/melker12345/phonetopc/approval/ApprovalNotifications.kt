package io.github.melker12345.phonetopc.approval

import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import androidx.core.app.NotificationChannelCompat
import androidx.core.app.NotificationCompat
import androidx.core.app.NotificationManagerCompat
import io.github.melker12345.phonetopc.R

/**
 * The heads-up notification of APPROVAL.md §4.2: channel "Approval requests", title
 * "<PC name> asks for approval" (or "asks to decrypt", DECRYPT.md §4), text = reason, actions
 * Approve and Deny.
 */
object ApprovalNotifications {
    const val CHANNEL_ID = "approval"
    private const val NOTIFICATION_ID = 10

    fun ensureChannel(context: Context) {
        val channel = NotificationChannelCompat.Builder(CHANNEL_ID, NotificationManagerCompat.IMPORTANCE_HIGH)
            .setName(context.getString(R.string.approval_channel_name))
            .setDescription(context.getString(R.string.approval_channel_description))
            .setShowBadge(false)
            .build()
        NotificationManagerCompat.from(context).createNotificationChannel(channel)
    }

    /** Whether an approval notification would be shown at all (permission, app and channel). */
    fun canShow(context: Context): Boolean {
        val nm = NotificationManagerCompat.from(context)
        if (!nm.areNotificationsEnabled()) return false
        val channel = nm.getNotificationChannelCompat(CHANNEL_ID) ?: return true // created on show
        return channel.importance != NotificationManagerCompat.IMPORTANCE_NONE
    }

    /** `false` if the user blocked it, so nobody would see it. */
    fun show(context: Context, request: ApprovalPrompts.Prompt): Boolean {
        ensureChannel(context)
        if (!canShow(context)) return false
        val id = request.id
        val open = PendingIntent.getActivity(
            context, REQUEST_OPEN,
            ApprovalActivity.intent(context, id).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        val approve = NotificationCompat.Action.Builder(0, context.getString(R.string.approval_approve), open)
            // On the lock screen, unlock first; the prompt is never shown over the keyguard.
            .setAuthenticationRequired(true)
            .build()
        val deny = NotificationCompat.Action.Builder(
            0, context.getString(R.string.approval_deny),
            ApprovalActionReceiver.pending(context, ApprovalActionReceiver.ACTION_DENY, id),
        ).build()
        val title = context.getString(
            if (request.decrypt) R.string.decrypt_notification_title else R.string.approval_notification_title,
            request.pcName,
        )
        // What the lock screen shows unless the user allows sensitive content there.
        val public = NotificationCompat.Builder(context, CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_phonetopc)
            .setContentTitle(context.getString(R.string.approval_notification_public_title))
            .setContentText(context.getString(R.string.approval_notification_public_text))
            .build()
        val notification = NotificationCompat.Builder(context, CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_phonetopc)
            .setContentTitle(title)
            .setContentText(request.reason)
            .setStyle(NotificationCompat.BigTextStyle().bigText(request.reason))
            .setContentIntent(open)
            .setDeleteIntent(ApprovalActionReceiver.pending(context, ApprovalActionReceiver.ACTION_DISMISSED, id))
            .addAction(approve)
            .addAction(deny)
            .setPriority(NotificationCompat.PRIORITY_HIGH)
            .setCategory(NotificationCompat.CATEGORY_REMINDER)
            .setVisibility(NotificationCompat.VISIBILITY_PRIVATE)
            .setPublicVersion(public)
            .setAutoCancel(false)
            .setOnlyAlertOnce(true)
            // A safety net: the request itself ends (and cancels this) at its timeout.
            .setTimeoutAfter(request.timeoutMillis + TIMEOUT_SLACK_MS)
            .build()
        return try {
            NotificationManagerCompat.from(context).notify(NOTIFICATION_ID, notification)
            true
        } catch (_: SecurityException) {
            false // POST_NOTIFICATIONS revoked between the check and the call
        }
    }

    fun cancel(context: Context) = NotificationManagerCompat.from(context).cancel(NOTIFICATION_ID)

    private const val REQUEST_OPEN = 10
    private const val TIMEOUT_SLACK_MS = 2_000L
}
