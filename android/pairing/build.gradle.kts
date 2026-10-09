import org.jetbrains.kotlin.gradle.dsl.JvmTarget

// Pure Kotlin/JVM: the phone's side of pairing (SECURITY.md §4). The QR payload, the pairing
// state machine and a small pinned-TLS WebSocket client, all testable off-device.
plugins {
    alias(libs.plugins.kotlin.jvm)
}

java {
    sourceCompatibility = JavaVersion.VERSION_17
    targetCompatibility = JavaVersion.VERSION_17
}

kotlin {
    compilerOptions { jvmTarget.set(JvmTarget.JVM_17) }
}

dependencies {
    api(project(":protocol"))

    testImplementation(libs.junit)
    testImplementation(libs.kotlinx.coroutines.test)
}

// Tests read the test certificates in spec/conformance/vectors (outside this Gradle build).
tasks.named<Test>("test") {
    val conformance = rootProject.layout.projectDirectory.dir("../spec/conformance")
    inputs.dir(conformance).withPathSensitivity(PathSensitivity.RELATIVE).withPropertyName("conformance")
    systemProperty("phonetopc.conformanceDir", conformance.asFile.absolutePath)
}
