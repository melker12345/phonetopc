package io.github.melker12345.phonetopc.ui.devices

import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.rounded.Devices
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.tooling.preview.Preview
import io.github.melker12345.phonetopc.R
import io.github.melker12345.phonetopc.ui.components.EmptyState
import io.github.melker12345.phonetopc.ui.theme.PhonetopcTheme

/** Paired PCs (name, last seen, revoke) arrive with pairing; until then, an empty state. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun DevicesScreen(onPair: () -> Unit) {
    Scaffold(topBar = { TopAppBar(title = { Text(stringResource(R.string.devices_title)) }) }) { padding ->
        Box(Modifier.fillMaxSize().padding(padding), contentAlignment = Alignment.Center) {
            EmptyState(
                icon = Icons.Rounded.Devices,
                title = stringResource(R.string.devices_empty_title),
                body = stringResource(R.string.devices_empty_body),
                action = { OutlinedButton(onClick = onPair) { Text(stringResource(R.string.devices_pair_action)) } },
            )
        }
    }
}

@Preview(showBackground = true)
@Composable
private fun DevicesPreview() = PhonetopcTheme(dynamicColor = false) { DevicesScreen(onPair = {}) }
