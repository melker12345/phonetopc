plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.compose)
}

android {
    namespace = "io.github.melker12345.phonetopc"
    compileSdk = 36

    defaultConfig {
        applicationId = "io.github.melker12345.phonetopc"
        minSdk = 26
        targetSdk = 36
        versionCode = 1
        versionName = "0.1.0"
    }

    buildTypes {
        release {
            isMinifyEnabled = false
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"), "proguard-rules.pro")
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    buildFeatures {
        compose = true
        buildConfig = true
    }

    packaging {
        resources {
            excludes += setOf(
                "/META-INF/{AL2.0,LGPL2.1}",
                "/META-INF/INDEX.LIST",
                "/META-INF/io.netty.versions.properties",
            )
        }
    }
}

dependencies {
    implementation(project(":server"))
    implementation(project(":pairing"))
    implementation(libs.kotlinx.coroutines.android)
    implementation(libs.androidx.datastore.preferences)
    implementation(libs.androidx.core.ktx)
    implementation(libs.androidx.activity.compose)
    implementation(libs.androidx.lifecycle.runtime.compose)
    implementation(libs.androidx.lifecycle.viewmodel.compose)
    implementation(libs.androidx.navigation.compose)
    implementation(platform(libs.androidx.compose.bom))
    implementation(libs.androidx.compose.ui)
    implementation(libs.androidx.compose.ui.graphics)
    implementation(libs.androidx.compose.ui.tooling.preview)
    implementation(libs.androidx.compose.material3)
    implementation(libs.androidx.compose.material.icons.extended)
    // QR scanning without Google Play Services (works on de-Googled phones and F-Droid).
    implementation(libs.androidx.camera.camera2)
    implementation(libs.androidx.camera.lifecycle)
    // camera-compose's viewfinder, not camera-view: camera-view pulls in camera-video, Media3
    // and Guava (several MB) that a QR scanner doesn't need.
    implementation(libs.androidx.camera.compose)
    implementation(libs.zxing.core)
    debugImplementation(libs.androidx.compose.ui.tooling)

    testImplementation(libs.junit)
}

// `./gradlew testDebugUnitTest` also runs the pure-JVM protocol and server tests.
tasks.matching { it.name == "testDebugUnitTest" }.configureEach {
    dependsOn(":protocol:test", ":server:test", ":pairing:test")
}
