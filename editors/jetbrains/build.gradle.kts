import org.jetbrains.intellij.platform.gradle.IntelliJPlatformType
import org.jetbrains.intellij.platform.gradle.TestFrameworkType

plugins {
    id("org.jetbrains.kotlin.jvm")
    id("org.jetbrains.intellij.platform")
}

group = providers.gradleProperty("pluginGroup").get()
version = providers.gradleProperty("pluginVersion").get()

kotlin {
    jvmToolchain(21)
    compilerOptions {
        // Inherit the platform interfaces' default methods instead of
        // generating overrides of them (which the Plugin Verifier reports as
        // uses of internal and experimental API, e.g. ToolWindowFactory.manage).
        jvmDefault.set(org.jetbrains.kotlin.gradle.dsl.JvmDefaultMode.NO_COMPATIBILITY)
    }
}

// src/uiTest: the plugin in a real IDE (a separate process with its own
// window), driven through its UI by the Starter and Driver frameworks.
// RW_UI_TEST=1 xvfb-run -a ./gradlew uiTest; it types and clicks with
// java.awt.Robot, so it wants a display of its own (CI: the ui job).
sourceSets {
    create("uiTest") {
        compileClasspath += sourceSets.main.get().output + sourceSets.test.get().output
        runtimeClasspath += sourceSets.main.get().output + sourceSets.test.get().output
    }
}

configurations.getByName("uiTestImplementation") {
    extendsFrom(configurations.testImplementation.get())
}

dependencies {
    testImplementation("junit:junit:4.13.2")

    intellijPlatform {
        intellijIdeaCommunity(providers.gradleProperty("platformVersion"))
        testFramework(TestFrameworkType.Platform)
        testFramework(TestFrameworkType.Starter, configurationName = "uiTestImplementation")
    }
    "uiTestImplementation"("org.junit.jupiter:junit-jupiter:5.14.4")
    "uiTestImplementation"("org.junit.platform:junit-platform-launcher:1.14.4")
    "uiTestImplementation"("org.kodein.di:kodein-di-jvm:7.33.0")
    "uiTestImplementation"("org.jetbrains.kotlinx:kotlinx-coroutines-core-jvm:1.10.2")
}

intellijPlatformTesting.testIdeUi.register("uiTest") {
    task {
        val uiTestSourceSet = sourceSets.getByName("uiTest")
        testClassesDirs = uiTestSourceSet.output.classesDirs
        classpath = uiTestSourceSet.runtimeClasspath
        useJUnitPlatform()
    }
}

intellijPlatform {
    pluginConfiguration {
        version = providers.gradleProperty("pluginVersion")
        ideaVersion {
            sinceBuild = "252"
            untilBuild = provider { null }
        }
    }
    pluginVerification {
        ides {
            create(IntelliJPlatformType.IntellijIdeaCommunity, "2025.2.6.3")
            create(IntelliJPlatformType.IntellijIdea, "2025.3.6.1")
            create(IntelliJPlatformType.IntellijIdea, "2026.2.3")
        }
    }
    // Only used when someone publishes (editors/jetbrains/README.md): read
    // from the environment, never stored in the repository.
    signing {
        certificateChain = providers.environmentVariable("CERTIFICATE_CHAIN")
        privateKey = providers.environmentVariable("PRIVATE_KEY")
        password = providers.environmentVariable("PRIVATE_KEY_PASSWORD")
    }
    publishing {
        token = providers.environmentVariable("PUBLISH_TOKEN")
    }
    buildSearchableOptions = false
    // No GUI Designer forms and no @NotNull to instrument.
    instrumentCode = false
}

// ./gradlew runLocalIde -PlocalIde=<folder of an installed IDE>: the plugin in
// that IDE, with its own sandbox (your IDE settings are not touched).
providers.gradleProperty("localIde").orNull?.let { ide ->
    intellijPlatformTesting.runIde.register("runLocalIde") {
        localPath = file(ide)
    }
}

tasks.test {
    // The integration tests start rw and agents: one test JVM at a time.
    maxParallelForks = 1
    testLogging {
        events("failed")
        exceptionFormat = org.gradle.api.tasks.testing.logging.TestExceptionFormat.FULL
    }
}
