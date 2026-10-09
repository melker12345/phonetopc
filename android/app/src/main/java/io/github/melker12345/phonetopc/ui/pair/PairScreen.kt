package io.github.melker12345.phonetopc.ui.pair

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.IntrinsicSize
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.rounded.Info
import androidx.compose.material.icons.rounded.Lan
import androidx.compose.material.icons.rounded.QrCodeScanner
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedCard
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.alpha
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.tooling.preview.Preview
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import io.github.melker12345.phonetopc.R
import io.github.melker12345.phonetopc.ui.components.IconBadge
import io.github.melker12345.phonetopc.ui.theme.PhonetopcTheme

/** Pairing arrives with the secure transport (M2). For now this is the shell of the screen. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun PairScreen() {
    Scaffold(topBar = { TopAppBar(title = { Text(stringResource(R.string.pair_title)) }) }) { padding ->
        Column(
            Modifier
                .fillMaxSize()
                .verticalScroll(rememberScrollState())
                .padding(padding)
                .padding(horizontal = 16.dp, vertical = 8.dp),
            verticalArrangement = Arrangement.spacedBy(16.dp),
        ) {
            Card(
                colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.secondaryContainer),
                shape = MaterialTheme.shapes.extraLarge,
            ) {
                Row(Modifier.padding(20.dp), verticalAlignment = Alignment.Top) {
                    Icon(Icons.Rounded.Info, null, Modifier.padding(top = 2.dp))
                    Spacer(Modifier.width(16.dp))
                    Column {
                        Text(stringResource(R.string.pair_soon_title), style = MaterialTheme.typography.titleMedium)
                        Spacer(Modifier.height(4.dp))
                        Text(stringResource(R.string.pair_soon_body), style = MaterialTheme.typography.bodyMedium)
                    }
                }
            }

            Row(
                Modifier.fillMaxWidth().height(IntrinsicSize.Min),
                horizontalArrangement = Arrangement.spacedBy(12.dp),
            ) {
                PairOption(
                    icon = Icons.Rounded.QrCodeScanner,
                    title = stringResource(R.string.pair_qr_title),
                    body = stringResource(R.string.pair_qr_body),
                    modifier = Modifier.weight(1f).fillMaxHeight(),
                )
                PairOption(
                    icon = Icons.Rounded.Lan,
                    title = stringResource(R.string.pair_network_title),
                    body = stringResource(R.string.pair_network_body),
                    modifier = Modifier.weight(1f).fillMaxHeight(),
                )
            }

            // Preview of the confirmation step, so the flow is recognisable once it lands.
            OutlinedCard(shape = MaterialTheme.shapes.extraLarge, modifier = Modifier.alpha(DISABLED_ALPHA)) {
                Column(
                    Modifier.fillMaxWidth().padding(24.dp),
                    horizontalAlignment = Alignment.CenterHorizontally,
                ) {
                    Text(stringResource(R.string.pair_code_label), style = MaterialTheme.typography.labelLarge)
                    Spacer(Modifier.height(8.dp))
                    Text(
                        "••• •••",
                        fontFamily = FontFamily.Monospace,
                        fontSize = 40.sp,
                        style = MaterialTheme.typography.displaySmall,
                    )
                    Spacer(Modifier.height(8.dp))
                    Text(
                        stringResource(R.string.pair_code_body),
                        style = MaterialTheme.typography.bodySmall,
                        textAlign = TextAlign.Center,
                    )
                }
            }
        }
    }
}

@Composable
private fun PairOption(icon: ImageVector, title: String, body: String, modifier: Modifier = Modifier) {
    // Visually present but disabled until pairing ships.
    OutlinedCard(
        onClick = {},
        enabled = false,
        shape = MaterialTheme.shapes.extraLarge,
        modifier = modifier,
    ) {
        Column(Modifier.padding(16.dp).alpha(DISABLED_ALPHA)) {
            IconBadge(icon, size = 48.dp)
            Spacer(Modifier.height(16.dp))
            Text(title, style = MaterialTheme.typography.titleMedium)
            Spacer(Modifier.height(4.dp))
            Text(body, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            Spacer(Modifier.height(12.dp))
            Surface(
                shape = MaterialTheme.shapes.small,
                color = MaterialTheme.colorScheme.surfaceContainerHighest,
            ) {
                Text(
                    stringResource(R.string.pair_coming_soon),
                    style = MaterialTheme.typography.labelMedium,
                    modifier = Modifier.padding(horizontal = 8.dp, vertical = 4.dp),
                )
            }
        }
    }
}

private const val DISABLED_ALPHA = 0.6f

@Preview(showBackground = true)
@Composable
private fun PairPreview() = PhonetopcTheme(dynamicColor = false) { PairScreen() }
