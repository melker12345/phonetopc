import org.jetbrains.kotlin.gradle.dsl.JvmTarget

// Pure Kotlin/JVM: protocol models, sensor catalog and the session dispatcher.
// Must not depend on Android so it can be unit-tested on the JVM.
plugins {
    alias(libs.plugins.kotlin.jvm)
    alias(libs.plugins.kotlin.serialization)
}

java {
    sourceCompatibility = JavaVersion.VERSION_17
    targetCompatibility = JavaVersion.VERSION_17
}

kotlin {
    compilerOptions { jvmTarget.set(JvmTarget.JVM_17) }
}

dependencies {
    api(libs.kotlinx.coroutines.core)
    api(libs.kotlinx.serialization.json)

    testImplementation(libs.junit)
    testImplementation(libs.kotlinx.coroutines.test)
}

// ServerCasesTest reads the shared suite in spec/conformance (outside this Gradle build).
tasks.named<Test>("test") {
    val conformance = rootProject.layout.projectDirectory.dir("../spec/conformance")
    inputs.dir(conformance).withPathSensitivity(PathSensitivity.RELATIVE).withPropertyName("conformance")
    systemProperty("phonetopc.conformanceDir", conformance.asFile.absolutePath)
}
