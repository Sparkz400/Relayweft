import org.jetbrains.intellij.platform.gradle.extensions.intellijPlatform

rootProject.name = "relayweft-jetbrains"

pluginManagement {
    plugins {
        id("org.jetbrains.kotlin.jvm") version "2.4.20"
    }
}

plugins {
    // Downloads a JDK 21 for the toolchain when none is installed (CI, fresh machines).
    id("org.gradle.toolchains.foojay-resolver-convention") version "1.0.0"
    id("org.jetbrains.intellij.platform.settings") version "2.19.0"
}

@Suppress("UnstableApiUsage")
dependencyResolutionManagement {
    repositories {
        mavenCentral()
        intellijPlatform {
            defaultRepositories()
        }
    }
}
