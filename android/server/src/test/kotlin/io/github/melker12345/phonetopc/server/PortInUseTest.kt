package io.github.melker12345.phonetopc.server

import io.github.melker12345.phonetopc.protocol.BridgeHub
import io.github.melker12345.phonetopc.protocol.Demand
import io.github.melker12345.phonetopc.protocol.DeviceInfo
import io.github.melker12345.phonetopc.protocol.Reading
import io.github.melker12345.phonetopc.protocol.ReadingSink
import io.github.melker12345.phonetopc.protocol.SensorCatalog
import io.github.melker12345.phonetopc.protocol.SensorInfo
import io.github.melker12345.phonetopc.protocol.SensorSource
import io.github.melker12345.phonetopc.protocol.ServerInfo
import io.github.melker12345.phonetopc.protocol.security.Pem
import io.github.melker12345.phonetopc.protocol.security.Pins
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Test
import java.io.File
import java.io.IOException
import java.net.InetAddress
import java.net.ServerSocket

/** The app tells "port in use" apart from other start failures with [isPortInUse]. */
class PortInUseTest {
    private class Source : SensorSource {
        override fun describe() = SensorCatalog.all.map { SensorInfo.of(it, true, 1.0, 200.0, 50.0) }
        override fun setDemand(source: String, demand: Demand?) {}
        override suspend fun read(source: String): Reading? = null
        override fun setSink(sink: ReadingSink?) {}
    }

    private val hub = BridgeHub(Source(), ServerInfo("phonetopc-android", "test"), { DeviceInfo("dev1", "Phone", platform = "android") })

    private val vectors = File(System.getProperty("phonetopc.conformanceDir") ?: "../../spec/conformance", "vectors")
    private val identity = TlsIdentity(
        Pem.ecPrivateKey(File(vectors, "phone-key.pem").readText()),
        Pins.parsePemCertificate(File(vectors, "phone-cert.pem").readText()),
    )

    private fun startOnTakenPort(tls: TlsIdentity?): Throwable =
        ServerSocket(0, 50, InetAddress.getByName("127.0.0.1")).use { taken ->
            val server = PhonetopcServer(hub, port = taken.localPort, host = "127.0.0.1", tls = tls)
            try {
                server.start()
                server.stop()
                fail("started on a port in use")
                throw AssertionError()
            } catch (e: Exception) {
                e
            }
        }

    @Test fun devModeOnATakenPortIsPortInUse() {
        val e = startOnTakenPort(tls = null)
        assertTrue("$e", isPortInUse(e))
    }

    @Test fun secureModeOnATakenPortIsPortInUse() {
        val e = startOnTakenPort(tls = identity)
        assertTrue("$e", isPortInUse(e))
    }

    @Test fun otherFailuresAreNot() {
        assertFalse(isPortInUse(IllegalStateException("already started")))
        assertFalse(isPortInUse(IOException("boom", IllegalArgumentException())))
        assertTrue(isPortInUse(IOException("wrapped", java.net.BindException("Address already in use"))))
    }
}
