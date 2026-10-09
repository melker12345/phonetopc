package io.github.melker12345.phonetopc.ui.pair

import android.util.Log
import androidx.camera.core.CameraSelector
import androidx.camera.core.ImageAnalysis
import androidx.camera.core.ImageProxy
import androidx.camera.compose.CameraXViewfinder
import androidx.camera.core.Preview
import androidx.camera.core.SurfaceRequest
import androidx.camera.lifecycle.ProcessCameraProvider
import androidx.camera.lifecycle.awaitInstance
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.runtime.setValue
import androidx.compose.foundation.layout.Box
import androidx.compose.ui.Modifier
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.platform.LocalContext
import androidx.core.content.ContextCompat
import androidx.lifecycle.compose.LocalLifecycleOwner
import com.google.zxing.BarcodeFormat
import com.google.zxing.BinaryBitmap
import com.google.zxing.DecodeHintType
import com.google.zxing.NotFoundException
import com.google.zxing.PlanarYUVLuminanceSource
import com.google.zxing.ReaderException
import com.google.zxing.common.HybridBinarizer
import com.google.zxing.qrcode.QRCodeReader
import java.util.concurrent.Executors
import java.util.concurrent.atomic.AtomicBoolean
import java.util.concurrent.atomic.AtomicReference

/**
 * Camera viewfinder that decodes QR codes with ZXing (no Google Play Services, so it works on
 * de-Googled phones). [onResult] runs on the main thread for every decoded code until it
 * returns `true`. Needs the CAMERA permission.
 */
@Composable
fun QrScanner(onResult: (String) -> Boolean, modifier: Modifier = Modifier) {
    val context = LocalContext.current
    val lifecycleOwner = LocalLifecycleOwner.current
    val currentOnResult by rememberUpdatedState(onResult)
    var surfaceRequest by remember { mutableStateOf<SurfaceRequest?>(null) }
    val executor = remember { Executors.newSingleThreadExecutor() }
    val done = remember { AtomicBoolean(false) }
    val bound = remember { AtomicReference<ProcessCameraProvider?>(null) }

    LaunchedEffect(lifecycleOwner) {
        val provider = runCatching { ProcessCameraProvider.awaitInstance(context) }
            .onFailure { Log.w(TAG, "no camera provider", it) }
            .getOrNull() ?: return@LaunchedEffect
        val preview = Preview.Builder().build().also { p -> p.setSurfaceProvider { request -> surfaceRequest = request } }
        val analysis = ImageAnalysis.Builder()
            .setBackpressureStrategy(ImageAnalysis.STRATEGY_KEEP_ONLY_LATEST)
            .build()
        val decoder = QrDecoder()
        val main = ContextCompat.getMainExecutor(context)
        analysis.setAnalyzer(executor) { image ->
            val text = image.use { decoder.decode(it) }
            if (text != null && !done.get()) {
                main.execute { if (!done.get() && currentOnResult(text)) done.set(true) }
            }
        }
        runCatching {
            provider.unbindAll()
            provider.bindToLifecycle(lifecycleOwner, CameraSelector.DEFAULT_BACK_CAMERA, preview, analysis)
            bound.set(provider)
        }.onFailure { Log.w(TAG, "camera bind failed", it) }
    }

    DisposableEffect(Unit) {
        onDispose {
            done.set(true)
            bound.getAndSet(null)?.let { runCatching { it.unbindAll() } }
            executor.shutdown()
        }
    }

    Box(modifier) {
        surfaceRequest?.let { CameraXViewfinder(surfaceRequest = it, contentScale = ContentScale.Crop) }
    }
}

/** ZXing over the luminance (Y) plane of a camera frame; QR codes don't care about rotation. */
private class QrDecoder {
    private val reader = QRCodeReader()
    private val hints = mapOf(
        DecodeHintType.POSSIBLE_FORMATS to listOf(BarcodeFormat.QR_CODE),
        DecodeHintType.TRY_HARDER to true,
        // Terminals often draw the code light-on-dark.
        DecodeHintType.ALSO_INVERTED to true,
    )
    private var buffer = ByteArray(0)

    fun decode(image: ImageProxy): String? {
        val plane = image.planes.firstOrNull() ?: return null
        val width = image.width
        val height = image.height
        val rowStride = plane.rowStride
        val data = plane.buffer
        val size = rowStride * height
        if (buffer.size != size) buffer = ByteArray(size)
        data.rewind()
        data.get(buffer, 0, minOf(data.remaining(), size))
        val source = PlanarYUVLuminanceSource(buffer, rowStride, height, 0, 0, width, height, false)
        return try {
            reader.decode(BinaryBitmap(HybridBinarizer(source)), hints).text
        } catch (_: NotFoundException) {
            null
        } catch (_: ReaderException) {
            null
        } finally {
            reader.reset()
        }
    }
}

private const val TAG = "QrScanner"
