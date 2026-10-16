package io.github.melker12345.phonetopc.approval

import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import io.github.melker12345.phonetopc.protocol.approval.ApprovalWhy

/** The approval notification's Deny action and its dismissal (APPROVAL.md §4.1). */
class ApprovalActionReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        val id = intent.getLongExtra(EXTRA_ID, -1)
        val why = when (intent.action) {
            ACTION_DENY -> ApprovalWhy.DENIED // without a prompt (§4.2)
            ACTION_DISMISSED -> ApprovalWhy.CANCELLED
            else -> return
        }
        ApprovalPrompts.finish(id, why)
    }

    companion object {
        const val ACTION_DENY = "io.github.melker12345.phonetopc.action.APPROVAL_DENY"
        const val ACTION_DISMISSED = "io.github.melker12345.phonetopc.action.APPROVAL_DISMISSED"
        private const val EXTRA_ID = "request_id"

        fun pending(context: Context, action: String, id: Long): PendingIntent = PendingIntent.getBroadcast(
            context,
            if (action == ACTION_DENY) 11 else 12,
            Intent(context, ApprovalActionReceiver::class.java).setAction(action).putExtra(EXTRA_ID, id),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
    }
}
