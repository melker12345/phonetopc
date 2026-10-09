package io.github.melker12345.phonetopc.ui

import androidx.annotation.StringRes
import androidx.compose.animation.fadeIn
import androidx.compose.animation.fadeOut
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.consumeWindowInsets
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.Devices
import androidx.compose.material.icons.outlined.Home
import androidx.compose.material.icons.outlined.Link
import androidx.compose.material.icons.outlined.Settings
import androidx.compose.material.icons.rounded.Devices
import androidx.compose.material.icons.rounded.Home
import androidx.compose.material.icons.rounded.Link
import androidx.compose.material.icons.rounded.Settings
import androidx.compose.material3.Icon
import androidx.compose.material3.NavigationBar
import androidx.compose.material3.NavigationBarItem
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.res.stringResource
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import androidx.navigation.NavDestination.Companion.hierarchy
import androidx.navigation.NavGraph.Companion.findStartDestination
import androidx.navigation.NavHostController
import androidx.navigation.compose.NavHost
import androidx.navigation.compose.composable
import androidx.navigation.compose.currentBackStackEntryAsState
import androidx.navigation.compose.rememberNavController
import io.github.melker12345.phonetopc.R
import io.github.melker12345.phonetopc.ui.devices.DevicesScreen
import io.github.melker12345.phonetopc.ui.home.HomeScreen
import io.github.melker12345.phonetopc.ui.onboarding.OnboardingScreen
import io.github.melker12345.phonetopc.ui.pair.PairScreen
import io.github.melker12345.phonetopc.ui.settings.SettingsScreen

private enum class Tab(
    val route: String,
    @param:StringRes val label: Int,
    val selectedIcon: ImageVector,
    val icon: ImageVector,
) {
    HOME("home", R.string.tab_home, Icons.Rounded.Home, Icons.Outlined.Home),
    PAIR("pair", R.string.tab_pair, Icons.Rounded.Link, Icons.Outlined.Link),
    DEVICES("devices", R.string.tab_devices, Icons.Rounded.Devices, Icons.Outlined.Devices),
    SETTINGS("settings", R.string.tab_settings, Icons.Rounded.Settings, Icons.Outlined.Settings),
}

@Composable
fun PhonetopcApp(vm: MainViewModel = viewModel()) {
    val settings by vm.settings.collectAsStateWithLifecycle()
    val loaded = settings ?: run {
        // DataStore hasn't answered yet (a few ms); keep the window background.
        Box(Modifier.fillMaxSize())
        return
    }
    if (!loaded.onboardingDone) {
        OnboardingScreen(onFinished = vm::completeOnboarding)
    } else {
        MainTabs(vm)
    }
}

@Composable
private fun MainTabs(vm: MainViewModel) {
    val nav = rememberNavController()
    val backStack by nav.currentBackStackEntryAsState()
    val destination = backStack?.destination

    Scaffold(
        // Each screen draws its own top bar and handles the status bar inset.
        contentWindowInsets = WindowInsets(0, 0, 0, 0),
        bottomBar = {
            NavigationBar {
                Tab.entries.forEach { tab ->
                    val selected = destination?.hierarchy?.any { it.route == tab.route } == true
                    NavigationBarItem(
                        selected = selected,
                        onClick = { nav.switchTo(tab.route) },
                        icon = { Icon(if (selected) tab.selectedIcon else tab.icon, contentDescription = null) },
                        label = { Text(stringResource(tab.label)) },
                    )
                }
            }
        },
    ) { padding ->
        NavHost(
            navController = nav,
            startDestination = Tab.HOME.route,
            modifier = Modifier.padding(padding).consumeWindowInsets(padding),
            enterTransition = { fadeIn() },
            exitTransition = { fadeOut() },
        ) {
            composable(Tab.HOME.route) { HomeScreen(vm, onPair = { nav.switchTo(Tab.PAIR.route) }) }
            composable(Tab.PAIR.route) {
                val settings by vm.settings.collectAsStateWithLifecycle()
                PairScreen(devMode = settings?.devMode == true, onOpenDevices = { nav.switchTo(Tab.DEVICES.route) })
            }
            composable(Tab.DEVICES.route) { DevicesScreen(vm, onPair = { nav.switchTo(Tab.PAIR.route) }) }
            composable(Tab.SETTINGS.route) { SettingsScreen(vm) }
        }
    }
}

/** Standard bottom-navigation behaviour: one copy of each tab, state restored on return. */
private fun NavHostController.switchTo(route: String) {
    navigate(route) {
        popUpTo(graph.findStartDestination().id) { saveState = true }
        launchSingleTop = true
        restoreState = true
    }
}
