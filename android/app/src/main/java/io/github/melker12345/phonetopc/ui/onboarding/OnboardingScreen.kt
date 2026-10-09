package io.github.melker12345.phonetopc.ui.onboarding

import android.Manifest
import android.os.Build
import androidx.activity.compose.BackHandler
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.animation.AnimatedContent
import androidx.compose.animation.animateColorAsState
import androidx.compose.animation.core.animateDpAsState
import androidx.compose.animation.fadeIn
import androidx.compose.animation.fadeOut
import androidx.compose.animation.slideInHorizontally
import androidx.compose.animation.slideOutHorizontally
import androidx.compose.animation.togetherWith
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.rounded.BatterySaver
import androidx.compose.material.icons.rounded.CheckCircle
import androidx.compose.material.icons.rounded.Notifications
import androidx.compose.material.icons.rounded.Sensors
import androidx.compose.material3.Button
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringArrayResource
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.tooling.preview.Preview
import androidx.compose.ui.unit.dp
import io.github.melker12345.phonetopc.R
import io.github.melker12345.phonetopc.ui.SystemSettings
import io.github.melker12345.phonetopc.ui.components.IconBadge
import io.github.melker12345.phonetopc.ui.settings.Vendor
import io.github.melker12345.phonetopc.ui.theme.PhonetopcTheme

private enum class Step { WELCOME, NOTIFICATIONS, BATTERY, DONE }

/** What it does → notification permission (13+) → battery exemption → done. Skippable. */
@Composable
fun OnboardingScreen(onFinished: () -> Unit) {
    val context = LocalContext.current
    val steps = remember {
        buildList {
            add(Step.WELCOME)
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU && !SystemSettings.notificationsGranted(context)) {
                add(Step.NOTIFICATIONS)
            }
            if (!SystemSettings.isIgnoringBatteryOptimizations(context)) add(Step.BATTERY)
            add(Step.DONE)
        }
    }
    var index by rememberSaveable { mutableIntStateOf(0) }
    val step = steps[index.coerceIn(steps.indices)]
    val next: () -> Unit = { if (index < steps.lastIndex) index++ else onFinished() }
    BackHandler(enabled = index > 0) { index-- }

    val permission = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) { next() }

    Scaffold { padding ->
        Column(Modifier.fillMaxSize().padding(padding).padding(horizontal = 24.dp)) {
            Row(Modifier.fillMaxWidth().height(56.dp), verticalAlignment = Alignment.CenterVertically) {
                Spacer(Modifier.weight(1f))
                if (step != Step.DONE) {
                    TextButton(onClick = onFinished) { Text(stringResource(R.string.onboarding_skip)) }
                }
            }

            AnimatedContent(
                targetState = step,
                transitionSpec = {
                    val forward = targetState.ordinal > initialState.ordinal
                    (slideInHorizontally { if (forward) it / 4 else -it / 4 } + fadeIn()) togetherWith
                        (slideOutHorizontally { if (forward) -it / 4 else it / 4 } + fadeOut())
                },
                modifier = Modifier.weight(1f),
                label = "onboardingStep",
            ) { s ->
                Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) {
                    StepContent(s)
                }
            }

            StepDots(count = steps.size, current = index, modifier = Modifier.align(Alignment.CenterHorizontally))
            Spacer(Modifier.height(24.dp))
            Button(
                onClick = {
                    when (step) {
                        Step.NOTIFICATIONS -> if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
                            permission.launch(Manifest.permission.POST_NOTIFICATIONS)
                        } else {
                            next()
                        }
                        Step.BATTERY -> {
                            SystemSettings.requestBatteryExemption(context)
                            next()
                        }
                        else -> next()
                    }
                },
                modifier = Modifier.fillMaxWidth().height(56.dp),
            ) {
                Text(stringResource(step.primaryAction()), style = MaterialTheme.typography.titleMedium)
            }
            Box(Modifier.fillMaxWidth().height(56.dp), contentAlignment = Alignment.Center) {
                if (step == Step.NOTIFICATIONS || step == Step.BATTERY) {
                    TextButton(onClick = next) { Text(stringResource(R.string.onboarding_not_now)) }
                }
            }
            Spacer(Modifier.height(8.dp))
        }
    }
}

@Composable
private fun StepContent(step: Step) {
    Column(
        Modifier.verticalScroll(rememberScrollState()).widthIn(max = 420.dp),
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        IconBadge(
            icon = step.icon(),
            size = 112.dp,
            container = MaterialTheme.colorScheme.primaryContainer,
            content = MaterialTheme.colorScheme.onPrimaryContainer,
        )
        Spacer(Modifier.height(32.dp))
        Text(
            stringResource(step.title()),
            style = MaterialTheme.typography.headlineMedium,
            textAlign = TextAlign.Center,
        )
        Spacer(Modifier.height(12.dp))
        Text(
            stringResource(step.body()),
            style = MaterialTheme.typography.bodyLarge,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
            textAlign = TextAlign.Center,
        )
        when (step) {
            Step.WELCOME -> {
                Spacer(Modifier.height(24.dp))
                Column(verticalArrangement = Arrangement.spacedBy(12.dp)) {
                    stringArrayResource(R.array.onboarding_welcome_points).forEach { point ->
                        Row(verticalAlignment = Alignment.Top) {
                            Icon(
                                Icons.Rounded.CheckCircle, null,
                                tint = MaterialTheme.colorScheme.primary,
                                modifier = Modifier.size(20.dp),
                            )
                            Spacer(Modifier.width(12.dp))
                            Text(point, style = MaterialTheme.typography.bodyMedium)
                        }
                    }
                }
            }
            Step.BATTERY -> {
                val vendor = remember { Vendor.detect() }
                if (vendor != Vendor.GENERIC) {
                    Spacer(Modifier.height(16.dp))
                    Text(
                        stringResource(R.string.onboarding_battery_vendor, stringResource(vendor.title)),
                        style = MaterialTheme.typography.bodyMedium,
                        color = MaterialTheme.colorScheme.primary,
                        textAlign = TextAlign.Center,
                    )
                }
            }
            else -> Unit
        }
    }
}

@Composable
private fun StepDots(count: Int, current: Int, modifier: Modifier = Modifier) {
    Row(modifier, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
        repeat(count) { i ->
            val selected = i == current
            val width by animateDpAsState(if (selected) 24.dp else 8.dp, label = "dotWidth")
            val color by animateColorAsState(
                if (selected) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.outlineVariant,
                label = "dotColor",
            )
            Box(Modifier.height(8.dp).width(width).clip(CircleShape).background(color))
        }
    }
}

private fun Step.icon(): ImageVector = when (this) {
    Step.WELCOME -> Icons.Rounded.Sensors
    Step.NOTIFICATIONS -> Icons.Rounded.Notifications
    Step.BATTERY -> Icons.Rounded.BatterySaver
    Step.DONE -> Icons.Rounded.CheckCircle
}

private fun Step.title(): Int = when (this) {
    Step.WELCOME -> R.string.onboarding_welcome_title
    Step.NOTIFICATIONS -> R.string.onboarding_notifications_title
    Step.BATTERY -> R.string.onboarding_battery_title
    Step.DONE -> R.string.onboarding_done_title
}

private fun Step.body(): Int = when (this) {
    Step.WELCOME -> R.string.onboarding_welcome_body
    Step.NOTIFICATIONS -> R.string.onboarding_notifications_body
    Step.BATTERY -> R.string.onboarding_battery_body
    Step.DONE -> R.string.onboarding_done_body
}

private fun Step.primaryAction(): Int = when (this) {
    Step.WELCOME -> R.string.onboarding_welcome_action
    Step.NOTIFICATIONS -> R.string.onboarding_notifications_action
    Step.BATTERY -> R.string.onboarding_battery_action
    Step.DONE -> R.string.onboarding_done_action
}

@Preview(showBackground = true)
@Composable
private fun OnboardingPreview() = PhonetopcTheme(dynamicColor = false) { OnboardingScreen(onFinished = {}) }
