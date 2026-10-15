package io.github.melker12345.phonetopc.ui

import android.Manifest
import android.annotation.SuppressLint
import android.app.Activity
import android.content.ActivityNotFoundException
import android.content.Context
import android.content.ContextWrapper
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import android.os.PowerManager
import android.provider.Settings
import androidx.biometric.BiometricManager
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.platform.LocalContext
import androidx.core.content.ContextCompat
import androidx.core.net.toUri
import androidx.lifecycle.compose.LifecycleResumeEffect

object SystemSettings {
    fun isIgnoringBatteryOptimizations(context: Context): Boolean {
        val pm = ContextCompat.getSystemService(context, PowerManager::class.java) ?: return true
        return pm.isIgnoringBatteryOptimizations(context.packageName)
    }

    /**
     * Ask to be exempt from battery optimization. phonetopc is a long-running bridge whose
     * whole job is to keep serving while the screen is off, which is an accepted use case
     * for this request.
     */
    @SuppressLint("BatteryLife")
    fun requestBatteryExemption(context: Context) {
        val direct = Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS, "package:${context.packageName}".toUri())
        try {
            context.startActivity(direct)
        } catch (_: ActivityNotFoundException) {
            // Some vendor ROMs remove the direct dialog; fall back to the list screen.
            runCatching { context.startActivity(Intent(Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS)) }
        }
    }

    fun notificationsGranted(context: Context): Boolean =
        Build.VERSION.SDK_INT < Build.VERSION_CODES.TIRAMISU ||
            ContextCompat.checkSelfPermission(context, Manifest.permission.POST_NOTIFICATIONS) == PackageManager.PERMISSION_GRANTED

    /** Opens fingerprint enrollment (strong biometrics), or the security screen on old Androids. */
    fun enrollBiometric(context: Context) {
        val intent = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
            Intent(Settings.ACTION_BIOMETRIC_ENROLL)
                .putExtra(Settings.EXTRA_BIOMETRIC_AUTHENTICATORS_ALLOWED, BiometricManager.Authenticators.BIOMETRIC_STRONG)
        } else {
            Intent(Settings.ACTION_SECURITY_SETTINGS)
        }
        try {
            context.startActivity(intent)
        } catch (_: ActivityNotFoundException) {
            runCatching { context.startActivity(Intent(Settings.ACTION_SECURITY_SETTINGS)) }
        }
    }

    fun openUrl(context: Context, url: String) {
        runCatching { context.startActivity(Intent(Intent.ACTION_VIEW, url.toUri())) }
    }

    /** This app's page in system settings, where a permanently denied permission can be granted. */
    fun openAppDetails(context: Context) {
        runCatching {
            context.startActivity(Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS, "package:${context.packageName}".toUri()))
        }
    }

    fun openLocationSettings(context: Context) {
        runCatching { context.startActivity(Intent(Settings.ACTION_LOCATION_SOURCE_SETTINGS)) }
    }

    fun openNfcSettings(context: Context) {
        try {
            context.startActivity(Intent(Settings.ACTION_NFC_SETTINGS))
        } catch (_: ActivityNotFoundException) {
            runCatching { context.startActivity(Intent(Settings.ACTION_WIRELESS_SETTINGS)) }
        }
    }
}

tailrec fun Context.findActivity(): Activity? = when (this) {
    is Activity -> this
    is ContextWrapper -> baseContext.findActivity()
    else -> null
}

/** Re-evaluates [read] every time the screen resumes (e.g. coming back from system settings). */
@Composable
fun <T> rememberOnResume(read: (Context) -> T): T {
    val context = LocalContext.current
    var value by remember { mutableStateOf(read(context)) }
    LifecycleResumeEffect(Unit) {
        value = read(context)
        onPauseOrDispose { }
    }
    return value
}
