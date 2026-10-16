package io.github.melker12345.phonetopc.approval

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.FilledTonalButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.window.Dialog
import androidx.compose.ui.window.DialogProperties
import io.github.melker12345.phonetopc.R

/**
 * The number choice before the fingerprint prompt (APPROVAL.md §4.2): who asks, the reason, and
 * the three numbers, one of which the PC shows.
 */
@Composable
fun NumberChoice(prompt: ApprovalPrompts.Prompt, onPick: (Int) -> Unit, onDeny: () -> Unit, onCancel: () -> Unit) {
    Dialog(onDismissRequest = onCancel, properties = DialogProperties(dismissOnClickOutside = false)) {
        Surface(
            shape = RoundedCornerShape(28.dp),
            color = MaterialTheme.colorScheme.surfaceContainerHigh,
            modifier = Modifier.widthIn(max = 420.dp),
        ) {
            Column(Modifier.padding(24.dp), horizontalAlignment = Alignment.CenterHorizontally) {
                Text(
                    stringResource(R.string.approval_prompt_title, prompt.program, prompt.pcName),
                    style = MaterialTheme.typography.labelLarge,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    textAlign = TextAlign.Center,
                )
                Spacer(Modifier.height(4.dp))
                Text(
                    stringResource(if (prompt.decrypt) R.string.approval_choice_decrypt else R.string.approval_choice_approve),
                    style = MaterialTheme.typography.headlineSmall,
                    textAlign = TextAlign.Center,
                )
                Spacer(Modifier.height(16.dp))
                Surface(
                    shape = RoundedCornerShape(16.dp),
                    color = MaterialTheme.colorScheme.surfaceContainerHighest,
                    modifier = Modifier.fillMaxWidth(),
                ) {
                    Text(prompt.reason, style = MaterialTheme.typography.bodyLarge, modifier = Modifier.padding(16.dp))
                }
                Spacer(Modifier.height(24.dp))
                Text(
                    stringResource(R.string.approval_pick_number),
                    style = MaterialTheme.typography.titleMedium,
                    textAlign = TextAlign.Center,
                )
                Spacer(Modifier.height(16.dp))
                Row(horizontalArrangement = Arrangement.spacedBy(12.dp), modifier = Modifier.fillMaxWidth()) {
                    for (n in prompt.choices) {
                        FilledTonalButton(
                            onClick = { onPick(n) },
                            shape = RoundedCornerShape(20.dp),
                            modifier = Modifier.weight(1f).height(80.dp),
                        ) {
                            Text(
                                n.toString(),
                                fontFamily = FontFamily.Monospace,
                                fontWeight = FontWeight.SemiBold,
                                fontSize = 32.sp,
                            )
                        }
                    }
                }
                Spacer(Modifier.height(16.dp))
                TextButton(onClick = onDeny, modifier = Modifier.fillMaxWidth()) {
                    Text(stringResource(R.string.approval_deny))
                }
            }
        }
    }
}
