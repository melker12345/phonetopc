package io.github.melker12345.phonetopc.approval

import android.app.Activity
import android.app.Application
import android.os.Bundle

/**
 * Whether one of the app's own screens (not the approval prompt) is resumed, i.e. the user is
 * looking at phonetopc right now. Then an approval may open its prompt directly
 * (APPROVAL.md §4.2), which Android also allows from the background only in this case.
 */
object AppVisibility : Application.ActivityLifecycleCallbacks {
    @Volatile private var resumed = 0

    val inForeground: Boolean get() = resumed > 0

    fun install(app: Application) = app.registerActivityLifecycleCallbacks(this)

    override fun onActivityResumed(activity: Activity) {
        if (activity !is ApprovalActivity) resumed++
    }

    override fun onActivityPaused(activity: Activity) {
        if (activity !is ApprovalActivity) resumed = (resumed - 1).coerceAtLeast(0)
    }

    override fun onActivityCreated(activity: Activity, savedInstanceState: Bundle?) = Unit
    override fun onActivityStarted(activity: Activity) = Unit
    override fun onActivityStopped(activity: Activity) = Unit
    override fun onActivitySaveInstanceState(activity: Activity, outState: Bundle) = Unit
    override fun onActivityDestroyed(activity: Activity) = Unit
}
