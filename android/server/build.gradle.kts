import org.jetbrains.kotlin.gradle.dsl.JvmTarget

// Ktor WebSocket transport for the protocol core. Plain JVM so it can be tested end to end
// with a real client off-device.
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
    implementation(libs.ktor.server.core)
    implementation(libs.ktor.server.cio)
    implementation(libs.ktor.server.websockets)

    testImplementation(libs.junit)
    testImplementation(project(":pairing")) // its pinned wss:// client
    testImplementation(libs.kotlinx.coroutines.test)
    testImplementation(libs.ktor.client.cio)
    testImplementation(libs.ktor.client.websockets)
    testRuntimeOnly(libs.slf4j.nop)
}

// ServerCasesCloseTest reads the shared suite in spec/conformance (outside this Gradle build).
tasks.named<Test>("test") {
    val conformance = rootProject.layout.projectDirectory.dir("../spec/conformance")
    inputs.dir(conformance).withPathSensitivity(PathSensitivity.RELATIVE).withPropertyName("conformance")
    systemProperty("phonetopc.conformanceDir", conformance.asFile.absolutePath)
}
