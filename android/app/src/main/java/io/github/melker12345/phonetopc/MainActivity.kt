package io.github.melker12345.phonetopc

import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import io.github.melker12345.phonetopc.ui.PhonetopcApp
import io.github.melker12345.phonetopc.ui.theme.PhonetopcTheme

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        enableEdgeToEdge()
        super.onCreate(savedInstanceState)
        setContent {
            PhonetopcTheme {
                PhonetopcApp()
            }
        }
    }
}
