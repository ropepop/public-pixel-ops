package lv.jolkins.pixelorchestrator.app

import android.util.Log
import kotlinx.coroutines.delay
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.withContext
import kotlinx.serialization.decodeFromString
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import lv.jolkins.pixelorchestrator.coreconfig.BootPath
import lv.jolkins.pixelorchestrator.coreconfig.HealthSnapshot
import lv.jolkins.pixelorchestrator.coreconfig.LegacyConfigCompat
import lv.jolkins.pixelorchestrator.coreconfig.StackConfigV1
import lv.jolkins.pixelorchestrator.coreconfig.StackPaths
import lv.jolkins.pixelorchestrator.health.HealthScope
import lv.jolkins.pixelorchestrator.health.RuntimeHealthChecker
import lv.jolkins.pixelorchestrator.rootexec.RootExecutor
import lv.jolkins.pixelorchestrator.runtimeinstaller.ArtifactEntry
import lv.jolkins.pixelorchestrator.runtimeinstaller.ArtifactManifest
import lv.jolkins.pixelorchestrator.runtimeinstaller.AssetProvider
import lv.jolkins.pixelorchestrator.runtimeinstaller.ComponentReleaseManifest
import lv.jolkins.pixelorchestrator.runtimeinstaller.ReleaseRollbackMetadata
import lv.jolkins.pixelorchestrator.runtimeinstaller.RuntimeInstallerControl
import lv.jolkins.pixelorchestrator.supervisor.SupervisorControl
import lv.jolkins.pixelorchestrator.coreconfig.StackStore
import java.time.Instant
import java.util.Base64
import kotlin.time.Duration

class OrchestratorFacade(
  private val stackStore: StackStore,
  private val rootExecutor: RootExecutor,
  private val runtimeInstaller: RuntimeInstallerControl,
  private val supervisor: SupervisorControl,
  private val healthChecker: RuntimeHealthChecker,
  private val assetProvider: AssetProvider,
  private val supportBundleExporter: SupportBundleExporting,
  private val json: Json = Json { prettyPrint = true; ignoreUnknownKeys = true; encodeDefaults = true }
) {
  private val bootstrapMutex = Mutex()

  suspend fun bootstrapStack(): FacadeOperationResult {
    requireRootAccess(operation = "bootstrap")?.let { return it }
    if (!bootstrapMutex.tryLock()) {
      return FacadeOperationResult(
        success = false,
        message = "Bootstrap is already running. Wait for completion and retry if needed."
      )
    }

    try {
      return runCatching {
        withContext(Dispatchers.IO) {
          logStep("bootstrap: begin")
          val config = loadExternalConfig()
          logStep("bootstrap: config loaded")
          stackStore.saveConfig(config)
          logStep("bootstrap: config saved")
          writeRuntimeEnvFiles(config)
          logStep("bootstrap: runtime env written")

          val manifest = loadArtifactManifest()
          logStep("bootstrap: artifact manifest loaded")
          val installResult = runtimeInstaller.bootstrap(
            config = config,
            assets = assetProvider,
            manifest = manifest,
            rootfsArtifactId = ROOTFS_ARTIFACT_ID
          )
          logStep("bootstrap: runtime installer completed success=${installResult.success}")

          if (!installResult.success) {
            return@withContext FacadeOperationResult(false, "Bootstrap failed: ${installResult.message}")
          }

          supervisor.startAll()
          logStep("bootstrap: supervisor start_all requested")
          val health = waitForBootstrapHealth(config)
          logStep("bootstrap: health gate evaluation completed")
          val healthGateOk = isBootstrapHealthGateOk(config, health)

          if (!healthGateOk) {
            return@withContext FacadeOperationResult(false, "Health gate failed after bootstrap", health)
          }

          val state = stackStore.loadStateOrDefault().copy(
            bootPath = BootPath.ANDROID_BOOT,
            lastHealthSnapshot = health
          )
          stackStore.saveState(state)

          FacadeOperationResult(
            success = true,
            message = "Bootstrap complete",
            healthSnapshot = health
          )
        }
      }.getOrElse { error ->
        FacadeOperationResult(
          success = false,
          message = "Bootstrap exception (${error::class.java.name}): ${error.message ?: "(no message)"}"
        )
      }
    } finally {
      bootstrapMutex.unlock()
    }
  }

  private suspend fun waitForBootstrapHealth(config: StackConfigV1): HealthSnapshot {
    val deadline = System.currentTimeMillis() + BOOTSTRAP_HEALTH_WAIT_MILLIS
    var health = runHealthCheck(HealthScope.FULL).healthSnapshot ?: HealthSnapshot()
    while (!isBootstrapHealthGateOk(config, health) && System.currentTimeMillis() < deadline) {
      val remainingSeconds = ((deadline - System.currentTimeMillis()) / 1000L).coerceAtLeast(0L)
      logStep(
        "bootstrap: health gate pending root=${health.rootGranted} dns=${health.dnsHealthy} ssh=${health.sshHealthy} " +
          "vpn=${health.vpnHealthy} train=${health.trainBotHealthy} satiksme=${health.satiksmeBotHealthy} notifier=${health.siteNotifierHealthy} " +
          "remote=${health.remoteHealthy} remaining=${remainingSeconds}s"
      )
      delay(BOOTSTRAP_HEALTH_RETRY_MILLIS)
      health = runHealthCheck(HealthScope.FULL).healthSnapshot ?: HealthSnapshot()
    }
    return health
  }

  private fun isBootstrapHealthGateOk(config: StackConfigV1, health: HealthSnapshot): Boolean {
    val vpnRequired = config.vpn.enabled || (config.modules["vpn"]?.enabled ?: false)
    val remoteHealthEnforced =
      (config.remote.dohEnabled || config.remote.dotEnabled) &&
        config.supervision.enforceRemoteListeners &&
        config.remote.watchdogEscalateRuntimeRestart
    return health.rootGranted &&
      health.dnsHealthy &&
      health.sshHealthy &&
      (!vpnRequired || health.vpnHealthy) &&
      health.trainBotHealthy &&
      health.satiksmeBotHealthy &&
      health.siteNotifierHealthy &&
      (!remoteHealthEnforced || health.remoteHealthy)
  }

  private fun logStep(message: String) {
    Log.i("OrchestratorFacade", message)
  }

  private fun normalizeDohEndpointMode(mode: String): String {
    return mode.trim().lowercase().ifBlank { "native" }
  }

  private fun modeRequiresToken(mode: String): Boolean {
    return mode == "tokenized" || mode == "dual"
  }

  private fun validateRemoteDohMode(config: StackConfigV1, mode: String) {
    if (mode !in setOf("native", "tokenized", "dual")) {
      error("Invalid remote.dohEndpointMode='$mode'. Allowed values: native, tokenized, dual.")
    }
    if (config.remote.dohEnabled && modeRequiresToken(mode)) {
      val token = config.remote.dohPathToken.trim()
      val tokenPattern = Regex("^[A-Za-z0-9._~-]{16,128}$")
      if (!tokenPattern.matches(token)) {
        error(
          "Invalid remote.dohPathToken for remote.dohEndpointMode='$mode'. " +
            "Use a token with 16-128 chars from [A-Za-z0-9._~-]."
        )
      }
    }
  }

  private fun validateRemoteDotIdentityConfig(config: StackConfigV1) {
    if (!config.remote.dotIdentityEnabled) {
      return
    }
    if (!config.remote.dotEnabled) {
      error("remote.dotIdentityEnabled=true requires remote.dotEnabled=true.")
    }

    val dotHostname = config.remote.dotHostname.trim().lowercase()
    if (dotHostname.isBlank()) {
      error("remote.dotIdentityEnabled=true requires remote.dotHostname to be set.")
    }

    val maxLabelLength = minOf(63, 253 - 1 - dotHostname.length)
    if (maxLabelLength < 1) {
      error("remote.dotHostname='$dotHostname' is too long to support wildcard identity hostnames.")
    }

    val labelLength = config.remote.dotIdentityLabelLength
    if (labelLength !in 1..maxLabelLength) {
      error(
        "Invalid remote.dotIdentityLabelLength='$labelLength'. " +
          "Expected a value between 1 and $maxLabelLength for remote.dotHostname='$dotHostname'."
      )
    }
  }

  private fun isValidIpv4(value: String): Boolean {
    val trimmed = value.trim()
    if (trimmed.isEmpty()) {
      return false
    }
    val parts = trimmed.split('.')
    if (parts.size != 4) {
      return false
    }
    return parts.all { part ->
      if (part.isEmpty() || part.length > 3 || !part.all(Char::isDigit)) {
        return@all false
      }
      val octet = part.toIntOrNull() ?: return@all false
      octet in 0..255
    }
  }

  private fun validateRouterAttributionConfig(config: StackConfigV1) {
    val routerLanIp = config.remote.routerLanIp.trim()
    if (config.remote.routerPublicIpAttributionEnabled) {
      if (!isValidIpv4(routerLanIp)) {
        error(
          "Invalid remote.routerLanIp='$routerLanIp' when remote.routerPublicIpAttributionEnabled=true. " +
            "Expected a valid IPv4 address (for example 192.168.0.1)."
        )
      }
      return
    }

    if (routerLanIp.isBlank()) {
      Log.w("OrchestratorFacade", "bootstrap: remote.routerLanIp is blank; router public IP attribution is disabled")
      return
    }
    if (!isValidIpv4(routerLanIp)) {
      Log.w(
        "OrchestratorFacade",
        "bootstrap: invalid remote.routerLanIp='$routerLanIp' ignored because remote.routerPublicIpAttributionEnabled=false"
      )
    }
  }

  private fun logDeprecatedRemoteConfig(config: StackConfigV1, dohEndpointMode: String) {
    val deprecatedFields = buildList {
      if (config.remote.dohInternalPort != 8053) add("remote.dohInternalPort")
      if (config.remote.dohRateLimitRps != 20) add("remote.dohRateLimitRps")
    }
    if (deprecatedFields.isNotEmpty()) {
      logStep(
        "bootstrap: deprecated remote fields are no-op and ignored: " +
          deprecatedFields.joinToString(", ")
      )
    }
    if (dohEndpointMode == "native" && config.remote.dohPathToken.isNotBlank()) {
      logStep("bootstrap: remote.dohPathToken is ignored when remote.dohEndpointMode=native")
    }
  }

  private fun ddnsRecordNames(config: StackConfigV1): String {
    val names = linkedSetOf<String>()
    val primary = config.ddns.recordName.trim()
    if (primary.isNotBlank()) {
      names.add(primary)
    }
    if (config.remote.dotIdentityEnabled) {
      val wildcard = config.remote.dotHostname.trim().lowercase().let { hostname ->
        if (hostname.isBlank()) "" else "*.$hostname"
      }
      if (wildcard.isNotBlank()) {
        names.add(wildcard)
      }
    }
    return names.joinToString(",")
  }

  suspend fun startAll(): FacadeOperationResult {
    requireRootAccess(operation = "start_all")?.let { return it }
    supervisor.startAll()
    return FacadeOperationResult(true, "Supervisor start requested")
  }

  suspend fun stopAll(): FacadeOperationResult {
    requireRootAccess(operation = "stop_all")?.let { return it }
    supervisor.stopAll()
    return FacadeOperationResult(true, "Supervisor stop requested")
  }

  suspend fun restart(component: String): FacadeOperationResult {
    return restartComponent(component)
  }

  suspend fun startComponent(component: String): FacadeOperationResult {
    val normalized = normalizeComponent(component)
      ?: return FacadeOperationResult(false, "Unknown component: $component")
    requireRootAccess(operation = "start_component:$normalized")?.let { return it }
    supervisor.startComponent(normalized)
    return waitForMutatedComponentHealth(action = "start_component", component = normalized)
  }

  suspend fun stopComponent(component: String): FacadeOperationResult {
    val normalized = normalizeComponent(component)
      ?: return FacadeOperationResult(false, "Unknown component: $component")
    requireRootAccess(operation = "stop_component:$normalized")?.let { return it }
    supervisor.stopComponent(normalized)
    return FacadeOperationResult(true, "Stop requested for $normalized")
  }

  suspend fun restartComponent(component: String): FacadeOperationResult {
    val normalized = normalizeComponent(component)
      ?: return FacadeOperationResult(false, "Unknown component: $component")
    requireRootAccess(operation = "restart_component:$normalized")?.let { return it }
    supervisor.restart(normalized)
    return waitForMutatedComponentHealth(action = "restart_component", component = normalized)
  }

  suspend fun redeployComponent(component: String): FacadeOperationResult {
    val normalized = normalizeComponent(component)
      ?: return FacadeOperationResult(false, "Unknown component: $component")
    requireRootAccess(operation = "redeploy_component:$normalized")?.let { return it }

    return runCatching {
      val spec = resolveRedeploySpec(normalized)
      val preMutation = supervisor.runHealthCheck(HealthScope.FULL)
      val config = loadExternalConfig()
      stackStore.saveConfig(config)
      writeTargetRuntimeInputs(config, spec.runtimeConfigComponent)

      val runtimeSync = runtimeInstaller.syncBundledRuntimeAssets(assetProvider, component = spec.runtimeAssetComponent)
      if (!runtimeSync.success) {
        return@runCatching FacadeOperationResult(
          success = false,
          message = "[runtime_asset_sync_failed] Runtime asset sync failed for redeploy_component:$normalized: ${runtimeSync.message}",
          healthSnapshot = preMutation
        )
      }

      if (spec.requiresQuiescentInstall) {
        val stopOutcome = stopAndQuiesceComponent(spec)
        if (!stopOutcome.success) {
          return@runCatching FacadeOperationResult(
            success = false,
            message = buildQuiescenceFailureMessage(spec, stopOutcome),
            healthSnapshot = preMutation
          )
        }
      }

      var installRollbackMetadata: ReleaseRollbackMetadata? = null
      if (spec.requiresReleaseManifest) {
        val releaseManifestComponent = spec.releaseManifestComponent ?: error("Missing release manifest component for $normalized")
        val releaseInstallComponent = spec.releaseInstallComponent ?: error("Missing release install component for $normalized")
        val releaseManifest = loadComponentReleaseManifest(releaseManifestComponent)
        val installResult = runtimeInstaller.installComponentRelease(
          config = config,
          component = releaseInstallComponent,
          manifest = releaseManifest
        )
        if (!installResult.success) {
          return@runCatching FacadeOperationResult(
            success = false,
            message = "[install_failed] Component release install failed for $normalized: ${installResult.message}",
            healthSnapshot = preMutation
          )
        }
        installRollbackMetadata = installResult.rollbackMetadata
        if (spec.rollbackStrategy == RollbackStrategy.PREVIOUS_CURRENT_RELEASE && installRollbackMetadata == null) {
          return@runCatching FacadeOperationResult(
            success = false,
            message = "[install_failed] Component release install for $normalized completed without rollback metadata",
            healthSnapshot = preMutation
          )
        }
      }

      when (spec.runtimeAction) {
        "restart_component" -> supervisor.restart(spec.runtimeActionComponent)
        "sync_ddns" -> supervisor.syncDdnsNow()
        else -> error("Unsupported redeploy runtime action: ${spec.runtimeAction}")
      }

      val postMutation =
        waitForStabilizedHealth(
          scope = HealthScope.FULL,
          deadlineMillis = HEALTHCHECK_WAIT_MILLIS,
          retryMillis = HEALTHCHECK_RETRY_MILLIS
        ) { snapshot -> spec.healthGateComponents.all { gateComponent -> componentHealthy(snapshot, gateComponent) } }
      val gateHealthy = spec.healthGateComponents.all { gateComponent -> componentHealthy(postMutation, gateComponent) }
      val regressedNeighbors = detectNeighborRegressions(preMutation, postMutation, spec.targetComponents)
      if (gateHealthy && regressedNeighbors.isEmpty()) {
        pruneRedeployReleases(config, spec)
        return@runCatching FacadeOperationResult(
          success = true,
          message = buildRedeployMessage(spec, gateHealthy = true, regressedNeighbors = emptyList()),
          healthSnapshot = postMutation
        )
      }

      if (spec.rollbackStrategy == RollbackStrategy.PREVIOUS_CURRENT_RELEASE && installRollbackMetadata != null) {
        val rollbackOutcome = rollbackFailedRedeploy(spec, config, installRollbackMetadata)
        val rollbackCode = rollbackFailureCode(gateHealthy, regressedNeighbors)
        val rollbackMessage =
          if (rollbackOutcome.success) {
            "[$rollbackCode] Redeploy failed for ${spec.requestedComponent}; previous release restored (${rollbackOutcome.releaseId}). ${buildPostDeployIssues(gateHealthy, regressedNeighbors)}"
          } else {
            "[$rollbackCode] Redeploy failed for ${spec.requestedComponent}; rollback failed. ${buildPostDeployIssues(gateHealthy, regressedNeighbors)}; rollback_detail=${rollbackOutcome.message}"
          }
        return@runCatching FacadeOperationResult(
          success = false,
          message = rollbackMessage,
          healthSnapshot = rollbackOutcome.healthSnapshot ?: postMutation
        )
      }

      FacadeOperationResult(
        success = false,
        message = "[post_deploy_failed] Redeploy failed for ${spec.requestedComponent}: ${buildPostDeployIssues(gateHealthy, regressedNeighbors)}",
        healthSnapshot = postMutation
      )
    }.getOrElse { error ->
      FacadeOperationResult(
        success = false,
        message = "Redeploy exception (${error::class.java.name}): ${error.message ?: "(no message)"}"
      )
    }
  }

  suspend fun healthComponent(component: String): FacadeOperationResult {
    val normalized = normalizeComponent(component)
      ?: return FacadeOperationResult(false, "Unknown component: $component")
    val snapshot =
      waitForStabilizedHealth(
        scope = HealthScope.FULL,
        deadlineMillis = HEALTHCHECK_WAIT_MILLIS,
        retryMillis = HEALTHCHECK_RETRY_MILLIS
      ) { candidate -> componentHealthy(candidate, normalized) }
    val healthy = componentHealthy(snapshot, normalized)
    return FacadeOperationResult(
      success = healthy,
      message = "Health check complete for $normalized",
      healthSnapshot = snapshot
    )
  }

  suspend fun writeActionResult(pixelRunId: String, action: String, component: String, result: FacadeOperationResult) {
    val normalizedRunId = pixelRunId.trim()
    val normalizedAction = action.trim().lowercase()
    val normalizedComponent = component.trim().lowercase()
    if (normalizedRunId.isBlank() || normalizedAction.isBlank()) {
      return
    }

    val payload = OrchestratorActionResult(
      pixelRunId = normalizedRunId,
      action = normalizedAction,
      component = normalizedComponent,
      success = result.success,
      message = result.message,
      outputPath = result.outputPath,
      healthSnapshot = result.healthSnapshot,
      recordedAt = Instant.now().toString()
    )
    val encoded = Base64.getEncoder().encodeToString(
      json.encodeToString(OrchestratorActionResult.serializer(), payload).toByteArray(Charsets.UTF_8)
    )
    val targetPath = actionResultPath(normalizedRunId, normalizedAction, normalizedComponent)
    val script = buildString {
      appendLine("set -eu")
      appendLine("target=${singleQuote(targetPath)}")
      appendLine("target_dir=${singleQuote(StackPaths.ACTION_RESULTS)}")
      appendLine("tmp=\"${'$'}{target}.tmp\"")
      appendLine("mkdir -p \"${'$'}{target_dir}\"")
      appendLine("printf '%s' ${singleQuote(encoded)} | base64 -d > \"${'$'}{tmp}\"")
      appendLine("chmod 600 \"${'$'}{tmp}\"")
      appendLine("mv \"${'$'}{tmp}\" \"${'$'}{target}\"")
    }
    val writeResult = rootExecutor.runScript(script)
    if (!writeResult.ok) {
      Log.w(
        "OrchestratorFacade",
        "action_result_write_failed path=$targetPath stderr=${abbreviateForError(writeResult.stderr)} stdout=${abbreviateForError(writeResult.stdout)}"
      )
    }
  }

  suspend fun runHealthCheck(scope: HealthScope): FacadeOperationResult {
    val snapshot =
      waitForStabilizedHealth(
        scope = scope,
        deadlineMillis = HEALTHCHECK_WAIT_MILLIS,
        retryMillis = HEALTHCHECK_RETRY_MILLIS
      ) { candidate -> candidate.supervisorHealthy }
    return FacadeOperationResult(
      success = snapshot.supervisorHealthy,
      message = "Health check complete",
      healthSnapshot = snapshot
    )
  }

  private fun componentHealthy(snapshot: HealthSnapshot, component: String): Boolean {
    return snapshot.moduleHealth[component]?.healthy ?: when (component) {
      "dns" -> snapshot.dnsHealthy
      "ssh" -> snapshot.sshHealthy
      "vpn" -> snapshot.vpnHealthy
      "train_bot" -> snapshot.trainBotHealthy
      "satiksme_bot" -> snapshot.satiksmeBotHealthy
      "site_notifier" -> snapshot.siteNotifierHealthy
      "ddns" -> snapshot.ddnsHealthy
      "remote" -> snapshot.remoteHealthy
      else -> false
    }
  }

  private suspend fun waitForStabilizedHealth(
    scope: HealthScope,
    deadlineMillis: Long,
    retryMillis: Long,
    gate: (HealthSnapshot) -> Boolean
  ): HealthSnapshot {
    val deadline = System.currentTimeMillis() + deadlineMillis
    var health = supervisor.runHealthCheck(scope)
    while (!gate(health) && System.currentTimeMillis() < deadline) {
      val remainingSeconds = ((deadline - System.currentTimeMillis()) / 1000L).coerceAtLeast(0L)
      logStep(
        "health: pending root=${health.rootGranted} dns=${health.dnsHealthy} ssh=${health.sshHealthy} " +
          "vpn=${health.vpnHealthy} train=${health.trainBotHealthy} satiksme=${health.satiksmeBotHealthy} notifier=${health.siteNotifierHealthy} " +
          "remote=${health.remoteHealthy} remaining=${remainingSeconds}s"
      )
      delay(retryMillis)
      health = supervisor.runHealthCheck(scope)
    }
    return health
  }

  private suspend fun waitForMutatedComponentHealth(action: String, component: String): FacadeOperationResult {
    val snapshot =
      waitForStabilizedHealth(
        scope = HealthScope.FULL,
        deadlineMillis = HEALTHCHECK_WAIT_MILLIS,
        retryMillis = HEALTHCHECK_RETRY_MILLIS
      ) { candidate -> componentHealthy(candidate, component) }
    val healthy = componentHealthy(snapshot, component)
    return FacadeOperationResult(
      success = healthy,
      message =
        if (healthy) {
          "Component $component healthy after $action"
        } else {
          "Component $component failed health gate after $action"
        },
      healthSnapshot = snapshot
    )
  }

  private suspend fun stopAndQuiesceComponent(spec: RedeploySpec): StopAndQuiesceOutcome {
    supervisor.stopComponent(spec.stopComponent)
    val initial = waitForComponentQuiescence(spec)
    if (initial.quiescent) {
      return StopAndQuiesceOutcome(success = true)
    }
    if (spec.retryBudget <= 0 || spec.staleCleanupCommand.isBlank()) {
      return StopAndQuiesceOutcome(
        success = false,
        failureCode = "stop_timeout",
        detail = initial.detail
      )
    }

    val cleanupResult = rootExecutor.run(spec.staleCleanupCommand, timeout = Duration.parse("45s"))
    val retryProbe = waitForComponentQuiescence(spec)
    if (retryProbe.quiescent) {
      return StopAndQuiesceOutcome(success = true, cleanupAttempted = true)
    }
    if (!cleanupResult.ok) {
      return StopAndQuiesceOutcome(
        success = false,
        failureCode = "cleanup_retry_failed",
        detail = "cleanup_hook_failed stderr=${abbreviateForError(cleanupResult.stderr)} stdout=${abbreviateForError(cleanupResult.stdout)}",
        cleanupAttempted = true
      )
    }
    return StopAndQuiesceOutcome(
      success = false,
      failureCode = "cleanup_retry_failed",
      detail = retryProbe.detail,
      cleanupAttempted = true
    )
  }

  private suspend fun waitForComponentQuiescence(spec: RedeploySpec): QuiescenceProbe {
    if (!spec.requiresQuiescentInstall || spec.quiescenceProbeScript.isBlank()) {
      return QuiescenceProbe(quiescent = true, detail = "quiescence_not_required")
    }
    val deadline = System.currentTimeMillis() + QUIESCENCE_WAIT_MILLIS
    var probe = probeComponentQuiescence(spec)
    while (!probe.quiescent && System.currentTimeMillis() < deadline) {
      delay(QUIESCENCE_RETRY_MILLIS)
      probe = probeComponentQuiescence(spec)
    }
    return probe
  }

  private suspend fun probeComponentQuiescence(spec: RedeploySpec): QuiescenceProbe {
    if (spec.quiescenceProbeScript.isBlank()) {
      return QuiescenceProbe(quiescent = true, detail = "quiescence_not_required")
    }
    val result = rootExecutor.runScript(spec.quiescenceProbeScript, timeout = Duration.parse("15s"))
    if (!result.ok) {
      return QuiescenceProbe(
        quiescent = false,
        detail = "probe_failed stderr=${abbreviateForError(result.stderr)} stdout=${abbreviateForError(result.stdout)}"
      )
    }
    val detail = result.stdout.trim().ifBlank { "QUIESCENT" }
    return QuiescenceProbe(quiescent = detail == "QUIESCENT", detail = detail)
  }

  private fun buildQuiescenceFailureMessage(spec: RedeploySpec, outcome: StopAndQuiesceOutcome): String {
    val detail = outcome.detail.ifBlank { "component still running after stop" }
    return "[${outcome.failureCode}] Redeploy failed for ${spec.requestedComponent}: $detail"
  }

  private suspend fun rollbackFailedRedeploy(
    spec: RedeploySpec,
    config: StackConfigV1,
    rollbackMetadata: ReleaseRollbackMetadata
  ): RollbackOutcome {
    if (spec.requiresQuiescentInstall) {
      val quiescence = stopAndQuiesceComponent(spec)
      if (!quiescence.success) {
        return RollbackOutcome(
          success = false,
          releaseId = rollbackMetadata.releaseId,
          message = "rollback stop failed: ${quiescence.detail}"
        )
      }
    }

    val rollbackResult = runtimeInstaller.rollbackComponentRelease(
      config = config,
      component = spec.rollbackComponent,
      rollbackMetadata = rollbackMetadata
    )
    if (!rollbackResult.success) {
      return RollbackOutcome(
        success = false,
        releaseId = rollbackMetadata.releaseId,
        message = rollbackResult.message
      )
    }

    supervisor.startComponent(spec.runtimeActionComponent)
    val rollbackHealth =
      waitForStabilizedHealth(
        scope = HealthScope.FULL,
        deadlineMillis = HEALTHCHECK_WAIT_MILLIS,
        retryMillis = HEALTHCHECK_RETRY_MILLIS
      ) { snapshot -> spec.healthGateComponents.all { gateComponent -> componentHealthy(snapshot, gateComponent) } }
    val rollbackHealthy = spec.healthGateComponents.all { gateComponent -> componentHealthy(rollbackHealth, gateComponent) }
    return RollbackOutcome(
      success = rollbackHealthy,
      releaseId = rollbackMetadata.previousTargetPath.substringAfterLast('/').ifBlank { rollbackMetadata.releaseId },
      message =
        if (rollbackHealthy) {
          "previous release restored"
        } else {
          "rollback restored previous target but health gate still failed"
        },
      healthSnapshot = rollbackHealth
    )
  }

  private suspend fun pruneRedeployReleases(config: StackConfigV1, spec: RedeploySpec) {
    val releaseComponent = spec.releaseInstallComponent ?: return
    val pruneResult = runtimeInstaller.pruneComponentReleases(config, releaseComponent, keepReleases = 3)
    if (!pruneResult.success) {
      logStep("redeploy: prune warning component=$releaseComponent detail=${pruneResult.message}")
    }
  }

  private fun rollbackFailureCode(gateHealthy: Boolean, regressedNeighbors: List<String>): String {
    return when {
      !gateHealthy && regressedNeighbors.isNotEmpty() -> "post_deploy_failed_rolled_back"
      !gateHealthy -> "health_gate_failed_rolled_back"
      else -> "neighbor_regression_rolled_back"
    }
  }

  private fun buildPostDeployIssues(gateHealthy: Boolean, regressedNeighbors: List<String>): String {
    val issues = buildList {
      if (!gateHealthy) {
        add("health gate failed")
      }
      if (regressedNeighbors.isNotEmpty()) {
        add("healthy neighbors regressed: ${regressedNeighbors.joinToString(", ")}")
      }
    }
    return issues.joinToString("; ").ifBlank { "unknown post-deploy failure" }
  }

  suspend fun syncDdnsNow(): FacadeOperationResult {
    requireRootAccess(operation = "sync_ddns")?.let { return it }
    supervisor.syncDdnsNow()
    return FacadeOperationResult(true, "DDNS sync requested")
  }

  suspend fun exportSupportBundle(includeSecrets: Boolean): FacadeOperationResult {
    val config = stackStore.loadConfigOrDefault()
    val state = stackStore.loadStateOrDefault().copy(
      lastHealthSnapshot = healthChecker.check(config)
    )
    stackStore.saveState(state)

    val exported = supportBundleExporter.export(
      config = config,
      state = state,
      includeSecrets = includeSecrets
    )

    return FacadeOperationResult(
      success = true,
      message = "Support bundle exported",
      healthSnapshot = state.lastHealthSnapshot,
      outputPath = exported.absolutePath
    )
  }

  private suspend fun loadExternalConfig(): StackConfigV1 {
    val command = "if [ -f '${StackConfigPath.FILE}' ]; then cat '${StackConfigPath.FILE}'; fi"
    val result = rootExecutor.run(command)
    if (!result.ok) {
      error("Failed to read orchestrator config at ${StackConfigPath.FILE}: ${result.stderr}")
    }

    val raw = result.stdout.trim()
    if (raw.isBlank()) {
      error("Missing orchestrator config file: ${StackConfigPath.FILE}")
    }

    return runCatching {
      val migratedRaw = LegacyConfigCompat.migrateConfigJson(raw, json)
      json.decodeFromString<StackConfigV1>(migratedRaw)
    }.getOrElse { parseError ->
      error("Invalid orchestrator config JSON at ${StackConfigPath.FILE}: ${parseError.message}")
    }
  }

  private suspend fun loadArtifactManifest(): ArtifactManifest {
    val command = "if [ -f '${RuntimeManifestPath.FILE}' ]; then cat '${RuntimeManifestPath.FILE}'; fi"
    val result = rootExecutor.run(command)
    if (!result.ok) {
      error("Failed to read runtime manifest at ${RuntimeManifestPath.FILE}: ${result.stderr}")
    }

    val raw = result.stdout.trim()
    if (raw.isBlank()) {
      error(
        "Missing runtime manifest at ${RuntimeManifestPath.FILE}. " +
          "Stage artifacts first with deploy_orchestrator_apk.sh --runtime-bundle-dir."
      )
    }

    val manifest = runCatching {
      json.decodeFromString<ArtifactManifest>(raw)
    }.getOrElse { parseError ->
      error("Invalid runtime manifest JSON at ${RuntimeManifestPath.FILE}: ${parseError.message}")
    }

    validateManifest(manifest)
    return manifest
  }

  private suspend fun loadComponentReleaseManifest(component: String): ComponentReleaseManifest {
    val manifestPath = ComponentReleasePath.fileFor(component)
    val command = "if [ -f '${manifestPath}' ]; then cat '${manifestPath}'; fi"
    val result = rootExecutor.run(command)
    if (!result.ok) {
      error("Failed to read component release manifest at $manifestPath: ${result.stderr}")
    }

    val raw = result.stdout.trim()
    if (raw.isBlank()) {
      error(
        "Missing component release manifest at $manifestPath. " +
          "Stage a release first with deploy_orchestrator_apk.sh --component-release-dir."
      )
    }

    val manifest = runCatching {
      json.decodeFromString<ComponentReleaseManifest>(raw)
    }.getOrElse { parseError ->
      error("Invalid component release manifest JSON at $manifestPath: ${parseError.message}")
    }

    validateComponentReleaseManifest(component, manifest)
    return manifest
  }

  private fun validateManifest(manifest: ArtifactManifest) {
    if (manifest.signatureSchema.lowercase() != REQUIRED_SIGNATURE_SCHEMA) {
      error("Unsupported manifest signature schema: ${manifest.signatureSchema}")
    }
    if (manifest.manifestVersion.isBlank()) {
      error("Manifest version is required")
    }

    fun requireLocalUrl(entry: ArtifactEntry) {
      if (!isLocalArtifactUrl(entry.url)) {
        error(
          "Artifact ${entry.id} has unsupported url '${entry.url}'. " +
            "Use /absolute/path or file:///absolute/path and stage with deploy_orchestrator_apk.sh --runtime-bundle-dir."
        )
      }
    }

    REQUIRED_BOOTSTRAP_ARTIFACT_IDS.forEach { requiredId ->
      val entry = manifest.artifacts.firstOrNull { it.id == requiredId }
        ?: error("Missing required artifact: $requiredId")
      if (!entry.required) {
        error("Required artifact must set required=true: $requiredId")
      }
      requireLocalUrl(entry)
    }

    OPTIONAL_BOOTSTRAP_ARTIFACT_IDS.forEach { optionalId ->
      val entry = manifest.artifacts.firstOrNull { it.id == optionalId } ?: return@forEach
      if (!entry.required) {
        error("Bootstrap artifact must set required=true when present: $optionalId")
      }
      requireLocalUrl(entry)
    }
  }

  private fun validateComponentReleaseManifest(component: String, manifest: ComponentReleaseManifest) {
    if (manifest.signatureSchema.lowercase() != REQUIRED_SIGNATURE_SCHEMA) {
      error("Unsupported component release signature schema: ${manifest.signatureSchema}")
    }
    if (manifest.schema != 1) {
      error("Unsupported component release schema: ${manifest.schema}")
    }
    if (manifest.componentId != component) {
      error("Component release manifest targets ${manifest.componentId}, expected $component")
    }
    if (manifest.releaseId.isBlank()) {
      error("Component release id is required for $component")
    }
    if (manifest.artifacts.isEmpty()) {
      error("Component release artifacts are required for $component")
    }
    manifest.artifacts.forEach { entry ->
      if (!isLocalArtifactUrl(entry.url)) {
        error(
          "Component release artifact ${entry.id} has unsupported url '${entry.url}'. " +
            "Use /absolute/path or file:///absolute/path and stage with deploy_orchestrator_apk.sh --component-release-dir."
        )
      }
    }
  }

  private fun isLocalArtifactUrl(rawUrl: String): Boolean {
    val url = rawUrl.trim()
    if (url.isBlank()) {
      return false
    }
    if (url.startsWith("http://", ignoreCase = true) || url.startsWith("https://", ignoreCase = true)) {
      return false
    }
    if (url.regionMatches(0, "file://", 0, 7, ignoreCase = true)) {
      return url.substring(7).startsWith("/")
    }
    return url.startsWith("/")
  }

  private suspend fun writeRuntimeEnvFiles(config: StackConfigV1) {
    val dohEndpointMode = normalizeDohEndpointMode(config.remote.dohEndpointMode)
    validateRemoteDohMode(config, dohEndpointMode)
    validateRemoteDotIdentityConfig(config)
    validateRouterAttributionConfig(config)
    logDeprecatedRemoteConfig(config, dohEndpointMode)
    val ddnsRecordNames = ddnsRecordNames(config)
    val sshAuthMode = when (config.ssh.authMode.trim().lowercase()) {
      "key_only", "password_only", "key_password" -> config.ssh.authMode.trim().lowercase()
      else -> "key_password"
    }
    val sshAllowPassword = when (sshAuthMode) {
      "key_only" -> false
      "password_only" -> true
      else -> config.ssh.passwordAuthEnabled
    }
    val sshAllowKey = sshAuthMode != "password_only"
    val defaultTrainWebPublicBaseUrl = "https://train-bot.example.com"
    val trainWebPublicBaseUrl =
      config.trainBot.publicBaseUrl.trim().removeSuffix("/").ifBlank { defaultTrainWebPublicBaseUrl }
    val trainWebIngressMode = "cloudflare_tunnel"
    val trainWebDirectProxyEnabled = trainWebIngressMode == "direct"
    val trainWebTunnelEnabled = trainWebIngressMode == "cloudflare_tunnel"
    val trainWebTunnelCredentialsFile =
      config.trainBot.tunnelCredentialsFile.trim().ifBlank {
        "/data/local/pixel-stack/conf/apps/train-bot-cloudflared.json"
      }
    val trainBotSingleInstanceLockPath = "${config.trainBot.runtimeRoot}/run/train-bot.instance.lock"
    val defaultSatiksmeWebPublicBaseUrl = "https://satiksme-bot.example.com"
    val satiksmeWebPublicBaseUrl =
      config.satiksmeBot.publicBaseUrl.trim().removeSuffix("/").ifBlank { defaultSatiksmeWebPublicBaseUrl }
    val satiksmeWebIngressMode = when (config.satiksmeBot.ingressMode.trim().lowercase()) {
      "cloudflare_tunnel" -> "cloudflare_tunnel"
      else -> "direct"
    }
    val satiksmeWebDirectProxyEnabled = satiksmeWebIngressMode == "direct"
    val satiksmeWebTunnelEnabled = satiksmeWebIngressMode == "cloudflare_tunnel"
    val satiksmeWebTunnelCredentialsFile =
      config.satiksmeBot.tunnelCredentialsFile.trim().ifBlank {
        "/data/local/pixel-stack/conf/apps/satiksme-bot-cloudflared.json"
      }
    val satiksmeBotSingleInstanceLockPath = "${config.satiksmeBot.runtimeRoot}/run/satiksme-bot.instance.lock"
    val script = buildString {
      appendLine("set -eu")
      appendLine("mkdir -p /data/local/pixel-stack/bin /data/local/pixel-stack/conf /data/local/pixel-stack/conf/apps /data/local/pixel-stack/conf/ddns /data/local/pixel-stack/conf/adguardhome /data/local/pixel-stack/conf/vpn /data/local/pixel-stack/ssh/conf ${singleQuote(config.vpn.runtimeRoot)} ${singleQuote("${config.vpn.runtimeRoot}/conf")} ${singleQuote("${config.vpn.runtimeRoot}/logs")} ${singleQuote("${config.vpn.runtimeRoot}/run")} ${singleQuote("${config.vpn.runtimeRoot}/state")} ${singleQuote("${config.vpn.runtimeRoot}/bin")}")
      appendLine("cat > /data/local/pixel-stack/conf/adguardhome.env <<'EOF_PIHOLE'")
      appendLine("ADGUARDHOME_ROOTFS_PATH=${config.runtime.rootfsPath}")
      appendLine("PIHOLE_DNS_PORT=${config.dns.dnsPort}")
      appendLine("PIHOLE_ACTIVE_DNS_PORT=${config.dns.dnsPort}")
      appendLine("PIHOLE_WEB_PORT=${config.dns.webPort}")
      appendLine("PIHOLE_DOH_BACKEND=${config.dns.dohBackend}")
      appendLine("PIHOLE_DOH_PORT=${config.dns.dohPort}")
      appendLine("PIHOLE_DOH_UPSTREAM_1=${config.dns.dohUpstream1}")
      appendLine("PIHOLE_DOH_UPSTREAM_2=${config.dns.dohUpstream2}")
      appendLine("PIHOLE_REMOTE_DOH_ENABLED=${if (config.remote.dohEnabled) 1 else 0}")
      appendLine("ADGUARDHOME_REMOTE_DOH_ENDPOINT_MODE=${dohEndpointMode}")
      if (modeRequiresToken(dohEndpointMode)) {
        appendLine("ADGUARDHOME_REMOTE_DOH_PATH_TOKEN=${config.remote.dohPathToken}")
      }
      appendLine("ADGUARDHOME_REMOTE_ROUTER_PUBLIC_IP_ATTRIBUTION_ENABLED=${if (config.remote.routerPublicIpAttributionEnabled) 1 else 0}")
      appendLine("ADGUARDHOME_REMOTE_ROUTER_LAN_IP=${config.remote.routerLanIp.trim()}")
      appendLine("PIHOLE_REMOTE_DOT_ENABLED=${if (config.remote.dotEnabled) 1 else 0}")
      appendLine("ADGUARDHOME_REMOTE_DOT_IDENTITY_ENABLED=${if (config.remote.dotIdentityEnabled) 1 else 0}")
      appendLine("ADGUARDHOME_REMOTE_DOT_IDENTITY_LABEL_LENGTH=${config.remote.dotIdentityLabelLength}")
      appendLine("PIHOLE_REMOTE_HOSTNAME=${config.remote.hostname}")
      appendLine("PIHOLE_REMOTE_DOT_HOSTNAME=${config.remote.dotHostname}")
      appendLine("PIHOLE_REMOTE_HTTPS_PORT=${config.remote.httpsPort}")
      appendLine("PIHOLE_REMOTE_DOT_PORT=${config.remote.dotPort}")
      appendLine("PIHOLE_REMOTE_DOT_MAX_CONN_PER_IP=${config.remote.dotMaxConnPerIp}")
      appendLine("PIHOLE_REMOTE_DOT_PROXY_TIMEOUT_SECONDS=${config.remote.dotProxyTimeoutSeconds}")
      appendLine("ADGUARDHOME_REMOTE_ADMIN_ENABLED=${if (config.remote.adminEnabled) 1 else 0}")
      appendLine("ADGUARDHOME_ADMIN_USERNAME=${config.remote.adminUsername}")
      appendLine("ADGUARDHOME_ADMIN_PASSWORD_FILE=${config.remote.adminPasswordFile}")
      appendLine("ADGUARDHOME_IPINFO_LITE_TOKEN_FILE=${config.remote.ipinfoLiteTokenFile}")
      appendLine("ADGUARDHOME_ADMIN_ALLOW_CIDRS=${config.remote.adminAllowCidrs}")
      appendLine("PIHOLE_REMOTE_ACME_ENABLED=${if (config.remote.acmeEnabled) 1 else 0}")
      appendLine("PIHOLE_REMOTE_ACME_EMAIL=${config.remote.acmeEmail}")
      appendLine("PIHOLE_REMOTE_ACME_CF_TOKEN_FILE=${config.remote.acmeCfTokenFile}")
      appendLine("PIHOLE_REMOTE_ACME_RENEW_MIN_DAYS=${config.remote.acmeRenewMinDays}")
      appendLine("PIHOLE_REMOTE_WATCHDOG_ENABLED=${if (config.remote.watchdogEnabled) 1 else 0}")
      appendLine("PIHOLE_REMOTE_WATCHDOG_ESCALATE_RUNTIME=${if (config.remote.watchdogEscalateRuntimeRestart) 1 else 0}")
      appendLine("PIHOLE_REMOTE_WATCHDOG_INTERVAL=${config.remote.watchdogIntervalSeconds}")
      appendLine("PIHOLE_REMOTE_WATCHDOG_FAILS=${config.remote.watchdogFails}")
      appendLine("PIHOLE_REMOTE_WATCHDOG_COOLDOWN=${config.remote.watchdogCooldownSeconds}")
      appendLine("PIHOLE_SERVICE_HEALTH_POLL_SEC=${config.supervision.healthPollSeconds}")
      appendLine("PIHOLE_SERVICE_ENFORCE_REMOTE_LISTENERS=${if (config.supervision.enforceRemoteListeners) 1 else 0}")
      appendLine("PIHOLE_SERVICE_UNHEALTHY_FAILS=${config.supervision.unhealthyFails}")
      appendLine("PIHOLE_SERVICE_MAX_RAPID_RESTARTS=${config.supervision.maxRapidRestarts}")
      appendLine("PIHOLE_SERVICE_RAPID_WINDOW_SECONDS=${config.supervision.rapidWindowSeconds}")
      appendLine("PIHOLE_SERVICE_BACKOFF_SECONDS=${config.supervision.backoffInitialSeconds}")
      appendLine("PIHOLE_SERVICE_BACKOFF_MAX_SECONDS=${config.supervision.backoffMaxSeconds}")
      appendLine("EOF_PIHOLE")
      appendLine("chmod 600 /data/local/pixel-stack/conf/adguardhome.env")

      appendLine("cat > /data/local/pixel-stack/ssh/conf/dropbear.env <<'EOF_SSH'")
      appendLine("SSH_PORT=${config.ssh.port}")
      appendLine("SSH_BIND_ADDRESS=${config.ssh.bindAddress}")
      appendLine("SSH_AUTH_MODE=${sshAuthMode}")
      appendLine("SSH_PASSWORD_AUTH=${if (sshAllowPassword) 1 else 0}")
      appendLine("SSH_ALLOW_KEY_AUTH=${if (sshAllowKey) 1 else 0}")
      appendLine("SSH_KEEPALIVE_SEC=${config.ssh.keepAliveSeconds}")
      appendLine("SSH_IDLE_TIMEOUT_SEC=${config.ssh.idleTimeoutSeconds}")
      appendLine("SSH_RECV_WINDOW_BYTES=${config.ssh.receiveWindowBytes}")
      appendLine("SSH_WIFI_FORCE_LOW_LATENCY=${if (config.ssh.wifiForceLowLatencyMode) 1 else 0}")
      appendLine("SSH_WIFI_FORCE_HIPERF=${if (config.ssh.wifiForceHiPerfMode) 1 else 0}")
      appendLine("SERVICE_MAX_RAPID_RESTARTS=${config.ssh.maxRapidRestarts}")
      appendLine("SERVICE_RAPID_WINDOW_SEC=${config.ssh.rapidWindowSeconds}")
      appendLine("SERVICE_BACKOFF_INITIAL_SEC=${config.ssh.backoffInitialSeconds}")
      appendLine("SERVICE_BACKOFF_MAX_SEC=${config.ssh.backoffMaxSeconds}")
      appendLine("EOF_SSH")
      appendLine("chmod 600 /data/local/pixel-stack/ssh/conf/dropbear.env")

      appendLine("cat > /data/local/pixel-stack/conf/vpn/tailscale.env <<'EOF_VPN'")
      appendLine("VPN_ENABLED=${if (config.vpn.enabled) 1 else 0}")
      appendLine("VPN_RUNTIME_ROOT=${config.vpn.runtimeRoot}")
      appendLine("VPN_AUTH_KEY_FILE=${config.vpn.authKeyFile}")
      appendLine("VPN_INTERFACE_NAME=${config.vpn.interfaceName}")
      appendLine("VPN_HOSTNAME=${config.vpn.hostname}")
      appendLine("VPN_ADVERTISE_TAGS=${config.vpn.advertiseTags}")
      appendLine("VPN_ACCEPT_ROUTES=${if (config.vpn.acceptRoutes) 1 else 0}")
      appendLine("VPN_ACCEPT_DNS=${if (config.vpn.acceptDns) 1 else 0}")
      appendLine("SERVICE_MAX_RAPID_RESTARTS=${config.vpn.maxRapidRestarts}")
      appendLine("SERVICE_RAPID_WINDOW_SEC=${config.vpn.rapidWindowSeconds}")
      appendLine("SERVICE_BACKOFF_INITIAL_SEC=${config.vpn.backoffInitialSeconds}")
      appendLine("SERVICE_BACKOFF_MAX_SEC=${config.vpn.backoffMaxSeconds}")
      appendLine("EOF_VPN")
      appendLine("chmod 600 /data/local/pixel-stack/conf/vpn/tailscale.env")

      appendLine("cat > /data/local/pixel-stack/conf/ddns.env <<'EOF_DDNS'")
      appendLine("DDNS_ENABLED=${if (config.ddns.enabled) 1 else 0}")
      appendLine("DDNS_PROVIDER=${config.ddns.provider}")
      appendLine("DDNS_POLL_SECONDS=${config.ddns.intervalSeconds}")
      appendLine("DDNS_REQUIRE_STABLE_READS=${config.ddns.requireStableReads}")
      appendLine("DDNS_UPDATE_TTL=${config.ddns.ttl}")
      appendLine("DDNS_ZONE_NAME=${config.ddns.zoneName}")
      appendLine("DDNS_RECORD_NAME=${config.ddns.recordName}")
      appendLine("DDNS_RECORD_NAMES=${ddnsRecordNames}")
      appendLine("DDNS_UPDATE_IPV4=${if (config.ddns.updateIpv4) 1 else 0}")
      appendLine("DDNS_UPDATE_IPV6=${if (config.ddns.updateIpv6) 1 else 0}")
      appendLine("DDNS_CF_API_TOKEN_FILE=${config.ddns.tokenFile}")
      appendLine("PUBLIC_IP_DISCOVERY_V4_URLS=${config.ddns.ipv4DiscoveryUrls}")
      appendLine("PUBLIC_IP_DISCOVERY_V6_URLS=${config.ddns.ipv6DiscoveryUrls}")
      appendLine("EOF_DDNS")
      appendLine("chmod 600 /data/local/pixel-stack/conf/ddns.env")

      appendLine("if [ ! -f ${singleQuote(config.trainBot.envFile)} ]; then")
      appendLine("cat > ${singleQuote(config.trainBot.envFile)} <<'EOF_TRAIN'")
      appendLine("BOT_TOKEN=")
      appendLine("DB_PATH=${config.trainBot.runtimeRoot}/train_bot.db")
      appendLine("TZ=Europe/Riga")
      appendLine("SCHEDULE_DIR=${config.trainBot.scheduleDir}")
      appendLine("LONG_POLL_TIMEOUT=30")
      appendLine("HTTP_TIMEOUT_SEC=45")
      appendLine("DATA_RETENTION_HOURS=24")
      appendLine("REPORT_COOLDOWN_MINUTES=3")
      appendLine("REPORT_DEDUPE_SECONDS=90")
      appendLine("TRAIN_WEB_ENABLED=true")
      appendLine("TRAIN_WEB_BIND_ADDR=127.0.0.1")
      appendLine("TRAIN_WEB_PORT=9317")
      appendLine("TRAIN_WEB_PUBLIC_BASE_URL=${trainWebPublicBaseUrl}")
      appendLine("TRAIN_WEB_DIRECT_PROXY_ENABLED=${if (trainWebDirectProxyEnabled) "true" else "false"}")
      appendLine("TRAIN_WEB_TUNNEL_ENABLED=${if (trainWebTunnelEnabled) "true" else "false"}")
      appendLine("TRAIN_WEB_TUNNEL_CREDENTIALS_FILE=${trainWebTunnelCredentialsFile}")
      appendLine("TRAIN_WEB_SESSION_SECRET_FILE=/data/local/pixel-stack/conf/apps/train-bot-web-session-secret")
      appendLine("TRAIN_WEB_TELEGRAM_AUTH_MAX_AGE_SEC=300")
      appendLine("SINGLE_INSTANCE_LOCK_PATH=${trainBotSingleInstanceLockPath}")
      appendLine("SERVICE_MAX_RAPID_RESTARTS=${config.trainBot.maxRapidRestarts}")
      appendLine("SERVICE_RAPID_WINDOW_SEC=${config.trainBot.rapidWindowSeconds}")
      appendLine("SERVICE_BACKOFF_INITIAL_SEC=${config.trainBot.backoffInitialSeconds}")
      appendLine("SERVICE_BACKOFF_MAX_SEC=${config.trainBot.backoffMaxSeconds}")
      appendLine("EOF_TRAIN")
      appendLine("chmod 600 ${singleQuote(config.trainBot.envFile)}")
      appendLine("fi")
      append(
        buildTrainBotWebEnvUpsertScript(
          envFile = config.trainBot.envFile,
          trainWebPublicBaseUrl = trainWebPublicBaseUrl,
          ingressMode = trainWebIngressMode,
          tunnelCredentialsFile = trainWebTunnelCredentialsFile,
          singleInstanceLockPath = trainBotSingleInstanceLockPath
        )
      )

      appendLine("if [ ! -f ${singleQuote(config.satiksmeBot.envFile)} ]; then")
      appendLine("cat > ${singleQuote(config.satiksmeBot.envFile)} <<'EOF_SATIKSME'")
      appendLine("BOT_TOKEN=")
      appendLine("DB_PATH=${config.satiksmeBot.runtimeRoot}/satiksme_bot.db")
      appendLine("TZ=Europe/Riga")
      appendLine("LONG_POLL_TIMEOUT=30")
      appendLine("HTTP_TIMEOUT_SEC=40")
      appendLine("DATA_RETENTION_HOURS=24")
      appendLine("REPORT_VISIBILITY_MINUTES=30")
      appendLine("REPORT_COOLDOWN_MINUTES=3")
      appendLine("REPORT_DEDUPE_SECONDS=90")
      appendLine("REPORT_DUMP_CHAT=@satiksme_bot_reports")
      appendLine("REPORTS_CHANNEL_URL=https://t.me/satiksme_bot_reports")
      appendLine("SATIKSME_SOURCE_STOPS_URL=https://saraksti.rigassatiksme.lv/riga/stops.txt")
      appendLine("SATIKSME_SOURCE_ROUTES_URL=https://saraksti.rigassatiksme.lv/riga/routes.txt")
      appendLine("SATIKSME_SOURCE_GTFS_URL=https://data.gov.lv/dati/dataset/6d78358a-0095-4ce3-b119-6cde5d0ac54f/resource/c576c770-a01b-49b0-bdc4-0005a1ec5838/download/marsrutusaraksti02_2026.zip")
      appendLine("SATIKSME_LIVE_DEPARTURES_URL=https://saraksti.rigassatiksme.lv/departures2.php")
      appendLine("SATIKSME_CATALOG_MIRROR_DIR=${config.satiksmeBot.runtimeRoot}/data/catalog/source")
      appendLine("SATIKSME_CATALOG_OUTPUT_PATH=${config.satiksmeBot.runtimeRoot}/data/catalog/generated/catalog.json")
      appendLine("SATIKSME_CATALOG_REFRESH_HOURS=24")
      appendLine("SATIKSME_CLEANUP_INTERVAL_MINUTES=10")
      appendLine("SATIKSME_WEB_ENABLED=true")
      appendLine("SATIKSME_WEB_BIND_ADDR=127.0.0.1")
      appendLine("SATIKSME_WEB_PORT=9327")
      appendLine("SATIKSME_WEB_PUBLIC_BASE_URL=${satiksmeWebPublicBaseUrl}")
      appendLine("SATIKSME_WEB_DIRECT_PROXY_ENABLED=${if (satiksmeWebDirectProxyEnabled) "true" else "false"}")
      appendLine("SATIKSME_WEB_TUNNEL_ENABLED=${if (satiksmeWebTunnelEnabled) "true" else "false"}")
      appendLine("SATIKSME_WEB_TUNNEL_CREDENTIALS_FILE=${satiksmeWebTunnelCredentialsFile}")
      appendLine("SATIKSME_WEB_SESSION_SECRET_FILE=/data/local/pixel-stack/conf/apps/satiksme-bot-web-session-secret")
      appendLine("SATIKSME_WEB_TELEGRAM_AUTH_MAX_AGE_SEC=300")
      appendLine("SINGLE_INSTANCE_LOCK_PATH=${satiksmeBotSingleInstanceLockPath}")
      appendLine("SERVICE_MAX_RAPID_RESTARTS=${config.satiksmeBot.maxRapidRestarts}")
      appendLine("SERVICE_RAPID_WINDOW_SEC=${config.satiksmeBot.rapidWindowSeconds}")
      appendLine("SERVICE_BACKOFF_INITIAL_SEC=${config.satiksmeBot.backoffInitialSeconds}")
      appendLine("SERVICE_BACKOFF_MAX_SEC=${config.satiksmeBot.backoffMaxSeconds}")
      appendLine("EOF_SATIKSME")
      appendLine("chmod 600 ${singleQuote(config.satiksmeBot.envFile)}")
      appendLine("fi")
      append(
        buildSatiksmeBotWebEnvUpsertScript(
          envFile = config.satiksmeBot.envFile,
          publicBaseUrl = satiksmeWebPublicBaseUrl,
          ingressMode = satiksmeWebIngressMode,
          tunnelCredentialsFile = satiksmeWebTunnelCredentialsFile,
          singleInstanceLockPath = satiksmeBotSingleInstanceLockPath
        )
      )

      appendLine("if [ ! -f ${singleQuote(config.siteNotifier.envFile)} ]; then")
      appendLine("cat > ${singleQuote(config.siteNotifier.envFile)} <<'EOF_NOTIFIER'")
      appendLine("TELEGRAM_BOT_TOKEN=")
      appendLine("TELEGRAM_CHAT_ID=")
      appendLine("GRIBU_LOGIN_ID=")
      appendLine("GRIBU_LOGIN_PASSWORD=")
      appendLine("GRIBU_BASE_URL=https://www.gribu.lv")
      appendLine("GRIBU_CHECK_URL=/lv/messages")
      appendLine("GRIBU_LOGIN_PATH=/pieslegties")
      appendLine("CHECK_INTERVAL_SEC=60")
      appendLine("CHECK_INTERVAL_FAST_SEC=20")
      appendLine("CHECK_INTERVAL_IDLE_SEC=60")
      appendLine("CHECK_INTERVAL_ERROR_BACKOFF_MAX_SEC=180")
      appendLine("HTTP_TIMEOUT_SEC=20")
      appendLine("ERROR_ALERT_COOLDOWN_SEC=1800")
      appendLine("STATE_FILE=${config.siteNotifier.runtimeRoot}/state/state.json")
      appendLine("DAEMON_LOCK_FILE=${config.siteNotifier.runtimeRoot}/state/daemon.lock")
      appendLine("WATCHDOG_CHECK_SEC=10")
      appendLine("WATCHDOG_STALE_SEC=120")
      appendLine("SUPERVISOR_RESTART_BASE_SEC=2")
      appendLine("SUPERVISOR_RESTART_MAX_SEC=30")
      appendLine("PARSE_LOW_CONFIDENCE_DELTA_LIMIT=20")
      appendLine("ROUTE_DISCOVERY_TTL_SEC=21600")
      appendLine("PARSE_MIN_CONFIDENCE_BASELINE=0.8")
      appendLine("PARSE_MIN_CONFIDENCE_UPDATE=0.7")
      appendLine("PARSE_MIN_CONFIDENCE_ROUTE_SELECTION=0.7")
      appendLine("RUNTIME_CONTEXT_POLICY=orchestrator_root")
      appendLine("SITE_NOTIFIER_DNS_SERVER=1.1.1.1")
      appendLine("SITE_NOTIFIER_DNS_SERVER_ALT=1.0.0.1")
      appendLine("NOTIFIER_PYTHON_PATH=${config.siteNotifier.pythonPath}")
      appendLine("NOTIFIER_ENTRY_SCRIPT=${config.siteNotifier.entryScript}")
      appendLine("SERVICE_MAX_RAPID_RESTARTS=${config.siteNotifier.maxRapidRestarts}")
      appendLine("SERVICE_RAPID_WINDOW_SEC=${config.siteNotifier.rapidWindowSeconds}")
      appendLine("SERVICE_BACKOFF_INITIAL_SEC=${config.siteNotifier.backoffInitialSeconds}")
      appendLine("SERVICE_BACKOFF_MAX_SEC=${config.siteNotifier.backoffMaxSeconds}")
      appendLine("EOF_NOTIFIER")
      appendLine("chmod 600 ${singleQuote(config.siteNotifier.envFile)}")
      appendLine("fi")
    }

    runRetriedRootScript("write runtime env files", script)
  }

  private suspend fun writeTargetRuntimeInputs(config: StackConfigV1, component: String) {
    when (component) {
      "dns" -> writeDnsRuntimeInputs(config)
      "ssh" -> writeSshRuntimeInputs(config)
      "vpn" -> writeVpnRuntimeInputs(config)
      "ddns" -> writeDdnsRuntimeInputs(config)
      "train_bot" -> writeTrainBotRuntimeInputs(config)
      "satiksme_bot" -> writeSatiksmeBotRuntimeInputs(config)
      "site_notifier" -> writeSiteNotifierRuntimeInputs(config)
      else -> error("Unsupported target runtime input component: $component")
    }
  }

  private suspend fun writeDnsRuntimeInputs(config: StackConfigV1) {
    val dohEndpointMode = normalizeDohEndpointMode(config.remote.dohEndpointMode)
    validateRemoteDohMode(config, dohEndpointMode)
    validateRemoteDotIdentityConfig(config)
    validateRouterAttributionConfig(config)
    logDeprecatedRemoteConfig(config, dohEndpointMode)
    val script = buildString {
      appendLine("set -eu")
      appendLine("mkdir -p /data/local/pixel-stack/conf /data/local/pixel-stack/conf/adguardhome")
      appendLine("cat > /data/local/pixel-stack/conf/adguardhome.env <<'EOF_PIHOLE'")
      appendLine("ADGUARDHOME_ROOTFS_PATH=${config.runtime.rootfsPath}")
      appendLine("PIHOLE_DNS_PORT=${config.dns.dnsPort}")
      appendLine("PIHOLE_ACTIVE_DNS_PORT=${config.dns.dnsPort}")
      appendLine("PIHOLE_WEB_PORT=${config.dns.webPort}")
      appendLine("PIHOLE_DOH_BACKEND=${config.dns.dohBackend}")
      appendLine("PIHOLE_DOH_PORT=${config.dns.dohPort}")
      appendLine("PIHOLE_DOH_UPSTREAM_1=${config.dns.dohUpstream1}")
      appendLine("PIHOLE_DOH_UPSTREAM_2=${config.dns.dohUpstream2}")
      appendLine("PIHOLE_REMOTE_DOH_ENABLED=${if (config.remote.dohEnabled) 1 else 0}")
      appendLine("ADGUARDHOME_REMOTE_DOH_ENDPOINT_MODE=${dohEndpointMode}")
      if (modeRequiresToken(dohEndpointMode)) {
        appendLine("ADGUARDHOME_REMOTE_DOH_PATH_TOKEN=${config.remote.dohPathToken}")
      }
      appendLine("ADGUARDHOME_REMOTE_ROUTER_PUBLIC_IP_ATTRIBUTION_ENABLED=${if (config.remote.routerPublicIpAttributionEnabled) 1 else 0}")
      appendLine("ADGUARDHOME_REMOTE_ROUTER_LAN_IP=${config.remote.routerLanIp.trim()}")
      appendLine("PIHOLE_REMOTE_DOT_ENABLED=${if (config.remote.dotEnabled) 1 else 0}")
      appendLine("ADGUARDHOME_REMOTE_DOT_IDENTITY_ENABLED=${if (config.remote.dotIdentityEnabled) 1 else 0}")
      appendLine("ADGUARDHOME_REMOTE_DOT_IDENTITY_LABEL_LENGTH=${config.remote.dotIdentityLabelLength}")
      appendLine("PIHOLE_REMOTE_HOSTNAME=${config.remote.hostname}")
      appendLine("PIHOLE_REMOTE_DOT_HOSTNAME=${config.remote.dotHostname}")
      appendLine("PIHOLE_REMOTE_HTTPS_PORT=${config.remote.httpsPort}")
      appendLine("PIHOLE_REMOTE_DOT_PORT=${config.remote.dotPort}")
      appendLine("PIHOLE_REMOTE_DOT_MAX_CONN_PER_IP=${config.remote.dotMaxConnPerIp}")
      appendLine("PIHOLE_REMOTE_DOT_PROXY_TIMEOUT_SECONDS=${config.remote.dotProxyTimeoutSeconds}")
      appendLine("ADGUARDHOME_REMOTE_ADMIN_ENABLED=${if (config.remote.adminEnabled) 1 else 0}")
      appendLine("ADGUARDHOME_ADMIN_USERNAME=${config.remote.adminUsername}")
      appendLine("ADGUARDHOME_ADMIN_PASSWORD_FILE=${config.remote.adminPasswordFile}")
      appendLine("ADGUARDHOME_IPINFO_LITE_TOKEN_FILE=${config.remote.ipinfoLiteTokenFile}")
      appendLine("ADGUARDHOME_ADMIN_ALLOW_CIDRS=${config.remote.adminAllowCidrs}")
      appendLine("PIHOLE_REMOTE_ACME_ENABLED=${if (config.remote.acmeEnabled) 1 else 0}")
      appendLine("PIHOLE_REMOTE_ACME_EMAIL=${config.remote.acmeEmail}")
      appendLine("PIHOLE_REMOTE_ACME_CF_TOKEN_FILE=${config.remote.acmeCfTokenFile}")
      appendLine("PIHOLE_REMOTE_ACME_RENEW_MIN_DAYS=${config.remote.acmeRenewMinDays}")
      appendLine("PIHOLE_REMOTE_WATCHDOG_ENABLED=${if (config.remote.watchdogEnabled) 1 else 0}")
      appendLine("PIHOLE_REMOTE_WATCHDOG_ESCALATE_RUNTIME=${if (config.remote.watchdogEscalateRuntimeRestart) 1 else 0}")
      appendLine("PIHOLE_REMOTE_WATCHDOG_INTERVAL=${config.remote.watchdogIntervalSeconds}")
      appendLine("PIHOLE_REMOTE_WATCHDOG_FAILS=${config.remote.watchdogFails}")
      appendLine("PIHOLE_REMOTE_WATCHDOG_COOLDOWN=${config.remote.watchdogCooldownSeconds}")
      appendLine("PIHOLE_SERVICE_HEALTH_POLL_SEC=${config.supervision.healthPollSeconds}")
      appendLine("PIHOLE_SERVICE_ENFORCE_REMOTE_LISTENERS=${if (config.supervision.enforceRemoteListeners) 1 else 0}")
      appendLine("PIHOLE_SERVICE_UNHEALTHY_FAILS=${config.supervision.unhealthyFails}")
      appendLine("PIHOLE_SERVICE_MAX_RAPID_RESTARTS=${config.supervision.maxRapidRestarts}")
      appendLine("PIHOLE_SERVICE_RAPID_WINDOW_SECONDS=${config.supervision.rapidWindowSeconds}")
      appendLine("PIHOLE_SERVICE_BACKOFF_SECONDS=${config.supervision.backoffInitialSeconds}")
      appendLine("PIHOLE_SERVICE_BACKOFF_MAX_SECONDS=${config.supervision.backoffMaxSeconds}")
      appendLine("EOF_PIHOLE")
      appendLine("chmod 600 /data/local/pixel-stack/conf/adguardhome.env")
    }
    runRetriedRootScript("write dns runtime inputs", script)
  }

  private suspend fun writeSshRuntimeInputs(config: StackConfigV1) {
    val sshAuthMode = when (config.ssh.authMode.trim().lowercase()) {
      "key_only", "password_only", "key_password" -> config.ssh.authMode.trim().lowercase()
      else -> "key_password"
    }
    val sshAllowPassword = when (sshAuthMode) {
      "key_only" -> false
      "password_only" -> true
      else -> config.ssh.passwordAuthEnabled
    }
    val sshAllowKey = sshAuthMode != "password_only"
    val script = buildString {
      appendLine("set -eu")
      appendLine("src_auth=${singleQuote(config.ssh.authorizedKeysSourceFile)}")
      appendLine("src_hash=${singleQuote(config.ssh.passwordHashSourceFile)}")
      appendLine("dst_auth=${singleQuote("${StackPaths.SSH}/home/root/.ssh/authorized_keys")}")
      appendLine("dst_passwd=${singleQuote("${StackPaths.SSH}/etc/passwd")}")
      appendLine("ssh_auth_mode=${singleQuote(sshAuthMode)}")
      appendLine("ssh_key_required=${if (sshAllowKey) "1" else "0"}")
      appendLine("mkdir -p /data/local/pixel-stack/ssh/conf ${singleQuote("${StackPaths.SSH}/home/root/.ssh")} ${singleQuote("${StackPaths.SSH}/etc")}")
      appendLine("cat > /data/local/pixel-stack/ssh/conf/dropbear.env <<'EOF_SSH'")
      appendLine("SSH_PORT=${config.ssh.port}")
      appendLine("SSH_BIND_ADDRESS=${config.ssh.bindAddress}")
      appendLine("SSH_AUTH_MODE=${sshAuthMode}")
      appendLine("SSH_PASSWORD_AUTH=${if (sshAllowPassword) 1 else 0}")
      appendLine("SSH_ALLOW_KEY_AUTH=${if (sshAllowKey) 1 else 0}")
      appendLine("SSH_KEEPALIVE_SEC=${config.ssh.keepAliveSeconds}")
      appendLine("SSH_IDLE_TIMEOUT_SEC=${config.ssh.idleTimeoutSeconds}")
      appendLine("SSH_RECV_WINDOW_BYTES=${config.ssh.receiveWindowBytes}")
      appendLine("SSH_WIFI_FORCE_LOW_LATENCY=${if (config.ssh.wifiForceLowLatencyMode) 1 else 0}")
      appendLine("SSH_WIFI_FORCE_HIPERF=${if (config.ssh.wifiForceHiPerfMode) 1 else 0}")
      appendLine("SERVICE_MAX_RAPID_RESTARTS=${config.ssh.maxRapidRestarts}")
      appendLine("SERVICE_RAPID_WINDOW_SEC=${config.ssh.rapidWindowSeconds}")
      appendLine("SERVICE_BACKOFF_INITIAL_SEC=${config.ssh.backoffInitialSeconds}")
      appendLine("SERVICE_BACKOFF_MAX_SEC=${config.ssh.backoffMaxSeconds}")
      appendLine("EOF_SSH")
      appendLine("chmod 600 /data/local/pixel-stack/ssh/conf/dropbear.env")
      appendLine("if [ \"\$ssh_key_required\" = \"1\" ] && [ ! -f \"\$src_auth\" ]; then")
      appendLine("  echo \"missing SSH authorized_keys source: \$src_auth\" >&2")
      appendLine("  exit 13")
      appendLine("fi")
      appendLine("if [ ! -f \"\$src_hash\" ]; then")
      appendLine("  echo \"missing SSH password hash source: \$src_hash\" >&2")
      appendLine("  exit 14")
      appendLine("fi")
      appendLine("if [ \"\$ssh_key_required\" = \"1\" ]; then")
      appendLine("  cp \"\$src_auth\" \"\$dst_auth\"")
      appendLine("  chmod 0600 \"\$dst_auth\"")
      appendLine("else")
      appendLine("  : > \"\$dst_auth\"")
      appendLine("  chmod 0600 \"\$dst_auth\"")
      appendLine("fi")
      appendLine("hash_line=\"\$(sed -n '/[^[:space:]]/ { s/^[[:space:]]*//; s/[[:space:]]*\$//; p; q; }' \"\$src_hash\")\"")
      appendLine("if [ -z \"\$hash_line\" ]; then")
      appendLine("  echo \"empty SSH password hash source: \$src_hash\" >&2")
      appendLine("  exit 15")
      appendLine("fi")
      appendLine("if printf '%s' \"\$hash_line\" | grep -Eq '^root:[^:]*:[^:]*:[^:]*:[^:]*:[^:]*:[^:]*\$'; then")
      appendLine("  passwd_line=\"\$hash_line\"")
      appendLine("elif printf '%s' \"\$hash_line\" | grep -Eq '^\\$6\\$'; then")
      appendLine("  passwd_line=\"root:\$hash_line:0:0:root:${StackPaths.SSH}/home/root:/system/bin/sh\"")
      appendLine("else")
      appendLine("  echo 'invalid SSH password hash format (expected \$6\$ hash or full passwd line)' >&2")
      appendLine("  exit 16")
      appendLine("fi")
      appendLine("printf '%s\\n' \"\$passwd_line\" > \"\$dst_passwd\"")
      appendLine("chmod 0600 \"\$dst_passwd\"")
    }
    runRetriedRootScript("write ssh runtime inputs", script)
  }

  private suspend fun writeVpnRuntimeInputs(config: StackConfigV1) {
    val script = buildString {
      appendLine("set -eu")
      appendLine("mkdir -p /data/local/pixel-stack/conf/vpn ${singleQuote(config.vpn.runtimeRoot)} ${singleQuote("${config.vpn.runtimeRoot}/conf")} ${singleQuote("${config.vpn.runtimeRoot}/logs")} ${singleQuote("${config.vpn.runtimeRoot}/run")} ${singleQuote("${config.vpn.runtimeRoot}/state")} ${singleQuote("${config.vpn.runtimeRoot}/bin")}")
      appendLine("cat > /data/local/pixel-stack/conf/vpn/tailscale.env <<'EOF_VPN'")
      appendLine("VPN_ENABLED=${if (config.vpn.enabled) 1 else 0}")
      appendLine("VPN_RUNTIME_ROOT=${config.vpn.runtimeRoot}")
      appendLine("VPN_AUTH_KEY_FILE=${config.vpn.authKeyFile}")
      appendLine("VPN_INTERFACE_NAME=${config.vpn.interfaceName}")
      appendLine("VPN_HOSTNAME=${config.vpn.hostname}")
      appendLine("VPN_ADVERTISE_TAGS=${config.vpn.advertiseTags}")
      appendLine("VPN_ACCEPT_ROUTES=${if (config.vpn.acceptRoutes) 1 else 0}")
      appendLine("VPN_ACCEPT_DNS=${if (config.vpn.acceptDns) 1 else 0}")
      appendLine("SERVICE_MAX_RAPID_RESTARTS=${config.vpn.maxRapidRestarts}")
      appendLine("SERVICE_RAPID_WINDOW_SEC=${config.vpn.rapidWindowSeconds}")
      appendLine("SERVICE_BACKOFF_INITIAL_SEC=${config.vpn.backoffInitialSeconds}")
      appendLine("SERVICE_BACKOFF_MAX_SEC=${config.vpn.backoffMaxSeconds}")
      appendLine("EOF_VPN")
      appendLine("chmod 600 /data/local/pixel-stack/conf/vpn/tailscale.env")
    }
    runRetriedRootScript("write vpn runtime inputs", script)
  }

  private suspend fun writeDdnsRuntimeInputs(config: StackConfigV1) {
    validateRemoteDotIdentityConfig(config)
    val ddnsRecordNames = ddnsRecordNames(config)
    val script = buildString {
      appendLine("set -eu")
      appendLine("mkdir -p /data/local/pixel-stack/conf")
      appendLine("cat > /data/local/pixel-stack/conf/ddns.env <<'EOF_DDNS'")
      appendLine("DDNS_ENABLED=${if (config.ddns.enabled) 1 else 0}")
      appendLine("DDNS_PROVIDER=${config.ddns.provider}")
      appendLine("DDNS_POLL_SECONDS=${config.ddns.intervalSeconds}")
      appendLine("DDNS_REQUIRE_STABLE_READS=${config.ddns.requireStableReads}")
      appendLine("DDNS_UPDATE_TTL=${config.ddns.ttl}")
      appendLine("DDNS_ZONE_NAME=${config.ddns.zoneName}")
      appendLine("DDNS_RECORD_NAME=${config.ddns.recordName}")
      appendLine("DDNS_RECORD_NAMES=${ddnsRecordNames}")
      appendLine("DDNS_UPDATE_IPV4=${if (config.ddns.updateIpv4) 1 else 0}")
      appendLine("DDNS_UPDATE_IPV6=${if (config.ddns.updateIpv6) 1 else 0}")
      appendLine("DDNS_CF_API_TOKEN_FILE=${config.ddns.tokenFile}")
      appendLine("PUBLIC_IP_DISCOVERY_V4_URLS=${config.ddns.ipv4DiscoveryUrls}")
      appendLine("PUBLIC_IP_DISCOVERY_V6_URLS=${config.ddns.ipv6DiscoveryUrls}")
      appendLine("EOF_DDNS")
      appendLine("chmod 600 /data/local/pixel-stack/conf/ddns.env")
    }
    runRetriedRootScript("write ddns runtime inputs", script)
  }

  private suspend fun writeTrainBotRuntimeInputs(config: StackConfigV1) {
    val defaultTrainWebPublicBaseUrl = "https://train-bot.example.com"
    val trainWebPublicBaseUrl =
      config.trainBot.publicBaseUrl.trim().removeSuffix("/").ifBlank { defaultTrainWebPublicBaseUrl }
    val trainWebIngressMode = "cloudflare_tunnel"
    val trainWebTunnelCredentialsFile =
      config.trainBot.tunnelCredentialsFile.trim().ifBlank {
        "/data/local/pixel-stack/conf/apps/train-bot-cloudflared.json"
      }
    val trainBotSingleInstanceLockPath = "${config.trainBot.runtimeRoot}/run/train-bot.instance.lock"
    val script = buildString {
      appendLine("set -eu")
      appendLine("mkdir -p /data/local/pixel-stack/conf/apps ${singleQuote("${config.trainBot.runtimeRoot}/env")} ${singleQuote("${config.trainBot.runtimeRoot}/data/schedules")} ${singleQuote("${config.trainBot.runtimeRoot}/logs")} ${singleQuote("${config.trainBot.runtimeRoot}/run")} ${singleQuote("${config.trainBot.runtimeRoot}/state")}")
      appendLine("if [ ! -f ${singleQuote(config.trainBot.envFile)} ]; then")
      appendLine("cat > ${singleQuote(config.trainBot.envFile)} <<'EOF_TRAIN'")
      appendLine("BOT_TOKEN=")
      appendLine("EOF_TRAIN")
      appendLine("chmod 600 ${singleQuote(config.trainBot.envFile)}")
      appendLine("fi")
      appendLine("cp ${singleQuote(config.trainBot.envFile)} ${singleQuote("${config.trainBot.runtimeRoot}/env/train-bot.env")}")
      appendLine("chmod 600 ${singleQuote("${config.trainBot.runtimeRoot}/env/train-bot.env")}")
      append(
        buildTrainBotRuntimeEnvUpsertScript(
          envFile = "${config.trainBot.runtimeRoot}/env/train-bot.env",
          runtimeRoot = config.trainBot.runtimeRoot,
          scheduleDir = config.trainBot.scheduleDir,
          trainWebPublicBaseUrl = trainWebPublicBaseUrl,
          ingressMode = trainWebIngressMode,
          tunnelCredentialsFile = trainWebTunnelCredentialsFile,
          singleInstanceLockPath = trainBotSingleInstanceLockPath,
          maxRapidRestarts = config.trainBot.maxRapidRestarts,
          rapidWindowSeconds = config.trainBot.rapidWindowSeconds,
          backoffInitialSeconds = config.trainBot.backoffInitialSeconds,
          backoffMaxSeconds = config.trainBot.backoffMaxSeconds
        )
      )
    }
    runRetriedRootScript("write train_bot runtime inputs", script)
  }

  private suspend fun writeSatiksmeBotRuntimeInputs(config: StackConfigV1) {
    val satiksmeWebPublicBaseUrl =
      config.satiksmeBot.publicBaseUrl.trim().removeSuffix("/").ifBlank { "https://satiksme-bot.example.com" }
    val satiksmeWebIngressMode = when (config.satiksmeBot.ingressMode.trim().lowercase()) {
      "cloudflare_tunnel" -> "cloudflare_tunnel"
      else -> "direct"
    }
    val satiksmeWebTunnelCredentialsFile =
      config.satiksmeBot.tunnelCredentialsFile.trim().ifBlank {
        "/data/local/pixel-stack/conf/apps/satiksme-bot-cloudflared.json"
      }
    val satiksmeBotSingleInstanceLockPath = "${config.satiksmeBot.runtimeRoot}/run/satiksme-bot.instance.lock"
    val script = buildString {
      appendLine("set -eu")
      appendLine("mkdir -p /data/local/pixel-stack/conf/apps ${singleQuote("${config.satiksmeBot.runtimeRoot}/env")} ${singleQuote("${config.satiksmeBot.runtimeRoot}/logs")} ${singleQuote("${config.satiksmeBot.runtimeRoot}/run")} ${singleQuote("${config.satiksmeBot.runtimeRoot}/state")} ${singleQuote("${config.satiksmeBot.runtimeRoot}/data/catalog")}")
      appendLine("if [ ! -f ${singleQuote(config.satiksmeBot.envFile)} ]; then")
      appendLine("cat > ${singleQuote(config.satiksmeBot.envFile)} <<'EOF_SATIKSME'")
      appendLine("BOT_TOKEN=")
      appendLine("REPORT_DUMP_CHAT=@satiksme_bot_reports")
      appendLine("EOF_SATIKSME")
      appendLine("chmod 600 ${singleQuote(config.satiksmeBot.envFile)}")
      appendLine("fi")
      appendLine("cp ${singleQuote(config.satiksmeBot.envFile)} ${singleQuote("${config.satiksmeBot.runtimeRoot}/env/satiksme-bot.env")}")
      appendLine("chmod 600 ${singleQuote("${config.satiksmeBot.runtimeRoot}/env/satiksme-bot.env")}")
      append(
        buildSatiksmeBotRuntimeEnvUpsertScript(
          envFile = "${config.satiksmeBot.runtimeRoot}/env/satiksme-bot.env",
          runtimeRoot = config.satiksmeBot.runtimeRoot,
          publicBaseUrl = satiksmeWebPublicBaseUrl,
          ingressMode = satiksmeWebIngressMode,
          tunnelCredentialsFile = satiksmeWebTunnelCredentialsFile,
          singleInstanceLockPath = satiksmeBotSingleInstanceLockPath,
          maxRapidRestarts = config.satiksmeBot.maxRapidRestarts,
          rapidWindowSeconds = config.satiksmeBot.rapidWindowSeconds,
          backoffInitialSeconds = config.satiksmeBot.backoffInitialSeconds,
          backoffMaxSeconds = config.satiksmeBot.backoffMaxSeconds
        )
      )
    }
    runRetriedRootScript("write satiksme_bot runtime inputs", script)
  }

  private suspend fun writeSiteNotifierRuntimeInputs(config: StackConfigV1) {
    val script = buildString {
      appendLine("set -eu")
      appendLine("mkdir -p /data/local/pixel-stack/conf/apps ${singleQuote("${config.siteNotifier.runtimeRoot}/env")} ${singleQuote("${config.siteNotifier.runtimeRoot}/logs")} ${singleQuote("${config.siteNotifier.runtimeRoot}/run")} ${singleQuote("${config.siteNotifier.runtimeRoot}/state")}")
      appendLine("if [ ! -f ${singleQuote(config.siteNotifier.envFile)} ]; then")
      appendLine("cat > ${singleQuote(config.siteNotifier.envFile)} <<'EOF_NOTIFIER'")
      appendLine("TELEGRAM_BOT_TOKEN=")
      appendLine("TELEGRAM_CHAT_ID=")
      appendLine("GRIBU_LOGIN_ID=")
      appendLine("GRIBU_LOGIN_PASSWORD=")
      appendLine("EOF_NOTIFIER")
      appendLine("chmod 600 ${singleQuote(config.siteNotifier.envFile)}")
      appendLine("fi")
      appendLine("cp ${singleQuote(config.siteNotifier.envFile)} ${singleQuote("${config.siteNotifier.runtimeRoot}/env/site-notifications.env")}")
      appendLine("chmod 600 ${singleQuote("${config.siteNotifier.runtimeRoot}/env/site-notifications.env")}")
      append(
        buildSiteNotifierRuntimeEnvUpsertScript(
          envFile = "${config.siteNotifier.runtimeRoot}/env/site-notifications.env",
          config = config.siteNotifier
        )
      )
    }
    runRetriedRootScript("write site_notifier runtime inputs", script)
  }

  private suspend fun runRetriedRootScript(operationName: String, script: String) {
    var failureDetail = ""
    repeat(RUNTIME_ENV_WRITE_MAX_ATTEMPTS) { attempt ->
      val result = rootExecutor.runScript(script)
      if (result.ok) {
        return
      }

      val attemptNumber = attempt + 1
      val stderr = abbreviateForError(result.stderr)
      val stdout = abbreviateForError(result.stdout)
      failureDetail =
        "attempt=$attemptNumber/$RUNTIME_ENV_WRITE_MAX_ATTEMPTS exit=${result.exitCode} stderr=$stderr stdout=$stdout"
      Log.w("OrchestratorFacade", "$operationName failed: $failureDetail")

      if (attemptNumber < RUNTIME_ENV_WRITE_MAX_ATTEMPTS) {
        delay(RUNTIME_ENV_WRITE_RETRY_MILLIS)
      }
    }

    error("Failed to $operationName: $failureDetail")
  }

  private fun resolveRedeploySpec(component: String): RedeploySpec {
    return when (component) {
      "dns" -> RedeploySpec(
        requestedComponent = component,
        runtimeConfigComponent = "dns",
        runtimeAssetComponent = "dns",
        releaseManifestComponent = "dns",
        releaseInstallComponent = "dns",
        runtimeAction = "restart_component",
        runtimeActionComponent = "dns",
        healthGateComponents = listOf("dns"),
        targetComponents = setOf("dns")
      )
      "ssh" -> RedeploySpec(
        requestedComponent = component,
        runtimeConfigComponent = "ssh",
        runtimeAssetComponent = "ssh",
        releaseManifestComponent = "ssh",
        releaseInstallComponent = "ssh",
        runtimeAction = "restart_component",
        runtimeActionComponent = "ssh",
        healthGateComponents = listOf("ssh"),
        targetComponents = setOf("ssh")
      )
      "vpn" -> RedeploySpec(
        requestedComponent = component,
        runtimeConfigComponent = "vpn",
        runtimeAssetComponent = "vpn",
        releaseManifestComponent = "vpn",
        releaseInstallComponent = "vpn",
        runtimeAction = "restart_component",
        runtimeActionComponent = "vpn",
        healthGateComponents = listOf("vpn"),
        targetComponents = setOf("vpn")
      )
      "ddns" -> RedeploySpec(
        requestedComponent = component,
        runtimeConfigComponent = "ddns",
        runtimeAssetComponent = "ddns",
        releaseManifestComponent = null,
        releaseInstallComponent = null,
        runtimeAction = "sync_ddns",
        runtimeActionComponent = "ddns",
        healthGateComponents = listOf("ddns"),
        targetComponents = setOf("ddns")
      )
      "train_bot" -> RedeploySpec(
        requestedComponent = component,
        runtimeConfigComponent = "train_bot",
        runtimeAssetComponent = "train_bot",
        releaseManifestComponent = "train_bot",
        releaseInstallComponent = "train_bot",
        runtimeAction = "restart_component",
        runtimeActionComponent = "train_bot",
        stopComponent = "train_bot",
        requiresQuiescentInstall = true,
        quiescenceProbeScript = trainBotQuiescenceProbeScript(),
        staleCleanupCommand = "sh /data/local/pixel-stack/bin/pixel-train-stop.sh",
        rollbackStrategy = RollbackStrategy.PREVIOUS_CURRENT_RELEASE,
        rollbackComponent = "train_bot",
        retryBudget = 1,
        healthGateComponents = listOf("train_bot"),
        targetComponents = setOf("train_bot")
      )
      "satiksme_bot" -> RedeploySpec(
        requestedComponent = component,
        runtimeConfigComponent = "satiksme_bot",
        runtimeAssetComponent = "satiksme_bot",
        releaseManifestComponent = "satiksme_bot",
        releaseInstallComponent = "satiksme_bot",
        runtimeAction = "restart_component",
        runtimeActionComponent = "satiksme_bot",
        stopComponent = "satiksme_bot",
        requiresQuiescentInstall = true,
        quiescenceProbeScript = satiksmeBotQuiescenceProbeScript(),
        staleCleanupCommand = "sh /data/local/pixel-stack/bin/pixel-satiksme-stop.sh",
        rollbackStrategy = RollbackStrategy.PREVIOUS_CURRENT_RELEASE,
        rollbackComponent = "satiksme_bot",
        retryBudget = 1,
        healthGateComponents = listOf("satiksme_bot"),
        targetComponents = setOf("satiksme_bot")
      )
      "site_notifier" -> RedeploySpec(
        requestedComponent = component,
        runtimeConfigComponent = "site_notifier",
        runtimeAssetComponent = "site_notifier",
        releaseManifestComponent = "site_notifier",
        releaseInstallComponent = "site_notifier",
        runtimeAction = "restart_component",
        runtimeActionComponent = "site_notifier",
        stopComponent = "site_notifier",
        requiresQuiescentInstall = true,
        quiescenceProbeScript = siteNotifierQuiescenceProbeScript(),
        staleCleanupCommand = "sh /data/local/pixel-stack/bin/pixel-notifier-stop.sh",
        rollbackStrategy = RollbackStrategy.PREVIOUS_CURRENT_RELEASE,
        rollbackComponent = "site_notifier",
        retryBudget = 1,
        healthGateComponents = listOf("site_notifier"),
        targetComponents = setOf("site_notifier")
      )
      "remote" -> RedeploySpec(
        requestedComponent = component,
        runtimeConfigComponent = "dns",
        runtimeAssetComponent = "dns",
        releaseManifestComponent = "dns",
        releaseInstallComponent = "dns",
        runtimeAction = "restart_component",
        runtimeActionComponent = "dns",
        healthGateComponents = listOf("dns", "remote"),
        targetComponents = setOf("dns", "remote")
      )
      else -> error("Unsupported redeploy component: $component")
    }
  }

  private fun detectNeighborRegressions(
    preMutation: HealthSnapshot,
    postMutation: HealthSnapshot,
    targetComponents: Set<String>
  ): List<String> {
    return SUPPORTED_COMPONENTS
      .filter { it !in targetComponents }
      .filter { componentHealthy(preMutation, it) && !componentHealthy(postMutation, it) }
  }

  private fun buildRedeployMessage(
    spec: RedeploySpec,
    gateHealthy: Boolean,
    regressedNeighbors: List<String>
  ): String {
    val issues = buildList {
      if (!gateHealthy) {
        add("health gate failed for ${spec.healthGateComponents.joinToString(", ")}")
      }
      if (regressedNeighbors.isNotEmpty()) {
        add("healthy neighbors regressed: ${regressedNeighbors.joinToString(", ")}")
      }
    }
    if (issues.isNotEmpty()) {
      return "Redeploy failed for ${spec.requestedComponent}: ${issues.joinToString("; ")}"
    }
    if (spec.requestedComponent == "remote") {
      return "Redeploy complete for remote via dns-owned release path"
    }
    return "Redeploy complete for ${spec.requestedComponent}"
  }

  private suspend fun requireRootAccess(operation: String): FacadeOperationResult? {
    if (rootExecutor.isRootAvailable()) {
      return null
    }
    return FacadeOperationResult(
      success = false,
      message = "Root access unavailable for $operation. Open Magisk/KernelSU/APatch and grant persistent superuser access to this app, then retry."
    )
  }

  private object StackConfigPath {
    const val FILE = "/data/local/pixel-stack/conf/orchestrator-config-v1.json"
  }

  private object RuntimeManifestPath {
    const val FILE = "/data/local/pixel-stack/conf/runtime/runtime-manifest.json"
  }

  private object ComponentReleasePath {
    private const val BASE = "/data/local/pixel-stack/conf/runtime/components"
    fun fileFor(component: String): String = "$BASE/$component/release-manifest.json"
  }

  private fun normalizeComponent(component: String): String? {
    val normalized = component.trim().lowercase()
    if (normalized in SUPPORTED_COMPONENTS) {
      return normalized
    }
    return null
  }

  private fun actionResultPath(pixelRunId: String, action: String, component: String): String {
    val suffix = component.ifBlank { "all" }
    return "${StackPaths.ACTION_RESULTS}/${pixelRunId}--${action}--${suffix}.json"
  }

  private fun singleQuote(value: String): String {
    return "'" + value.replace("'", "'\"'\"'") + "'"
  }

  private fun abbreviateForError(value: String): String {
    val normalized = value.trim().replace("\n", "\\n")
    if (normalized.isEmpty()) {
      return "<empty>"
    }
    if (normalized.length <= MAX_ERROR_FIELD_LENGTH) {
      return normalized
    }
    return normalized.take(MAX_ERROR_FIELD_LENGTH) + "..."
  }

  private fun siteNotifierQuiescenceProbeScript(): String {
    return """
      set -eu
      self_pid="${'$'}$"
      self_ppid="${'$'}PPID"
      details=""
      process_matches() {
        pattern="${'$'}1"
        target_base="$(basename "${'$'}pattern")"
        printf '%s\n' "${'$'}ps_output" | awk -v pat="${'$'}pattern" -v target_base="${'$'}target_base" -v self_pid="${'$'}self_pid" -v self_ppid="${'$'}self_ppid" '
          function starts_with(value, prefix) { return index(value, prefix) == 1 }
          function next_is_boundary(value, prefix_len) {
            c = substr(value, prefix_len + 1, 1)
            return c == "" || c == " "
          }
          {
            pid = $1
            name = $2
            if (pid == self_pid || pid == self_ppid) {
              next
            }
            args = ""
            if (NF >= 3) {
              args = substr($0, index($0, $3))
            }
            if (name == target_base) {
              found = 1
              exit
            }
            if (
              args == pat ||
              (starts_with(args, pat) && next_is_boundary(args, length(pat))) ||
              (starts_with(args, "sh " pat) && next_is_boundary(args, length("sh " pat))) ||
              (starts_with(args, target_base) && next_is_boundary(args, length(target_base)))
            ) {
              found = 1
              exit
            }
          }
          END { exit(found ? 0 : 1) }
        ' >/dev/null 2>&1
      }
      for pid_file in \
        /data/local/pixel-stack/apps/site-notifications/run/site-notifier-service-loop.pid \
        /data/local/pixel-stack/apps/site-notifications/run/site-notifier.pid; do
        if [ -s "${'$'}pid_file" ]; then
          pid="$(cat "${'$'}pid_file" 2>/dev/null || true)"
          if [ -n "${'$'}pid" ] && kill -0 "${'$'}pid" >/dev/null 2>&1; then
            details="${'$'}details pid_file=${'$'}pid_file:${'$'}pid;"
          fi
        fi
      done
      ps_output="$(ps -A -o PID=,NAME=,ARGS= 2>/dev/null || true)"
      for pattern in \
        /data/local/pixel-stack/apps/site-notifications/bin/site-notifier-service-loop \
        /data/local/pixel-stack/apps/site-notifications/bin/site-notifier-launch \
        /data/local/pixel-stack/apps/site-notifications/bin/site-notifier-python.current \
        /data/local/pixel-stack/apps/site-notifications/bin/site-notifier-python3.current; do
        if process_matches "${'$'}pattern"; then
          details="${'$'}details process=${'$'}pattern;"
        fi
      done
      if printf '%s\n' "${'$'}ps_output" | awk -v self_pid="${'$'}self_pid" -v self_ppid="${'$'}self_ppid" '
        {
          pid = $1
          name = $2
          args = ""
          if (NF >= 3) {
            args = substr($0, index($0, $3))
          }
          if (
            pid != self_pid &&
            pid != self_ppid &&
            name == "site-notifier-python3.current" &&
            index(args, "/data/local/pixel-stack/apps/site-notifications/current/app.py daemon") > 0
          ) {
            found = 1
          }
        }
        END { exit(found ? 0 : 1) }
      ' >/dev/null 2>&1; then
        details="${'$'}details process=current_app_daemon;"
      fi
      if printf '%s\n' "${'$'}ps_output" | awk -v self_pid="${'$'}self_pid" -v self_ppid="${'$'}self_ppid" '
        {
          pid = $1
          name = $2
          args = ""
          if (NF >= 3) {
            args = substr($0, index($0, $3))
          }
          if (
            pid != self_pid &&
            pid != self_ppid &&
            name == "site-notifier-python3.current" &&
            index(args, "/data/local/pixel-stack/apps/site-notifications/releases/") > 0 &&
            index(args, "app.py daemon") > 0
          ) {
            found = 1
          }
        }
        END { exit(found ? 0 : 1) }
      ' >/dev/null 2>&1; then
        details="${'$'}details process=release_app_daemon;"
      fi
      if [ -n "${'$'}details" ]; then
        printf '%s\n' "${'$'}details"
      else
        printf 'QUIESCENT\n'
      fi
    """.trimIndent()
  }

  private fun trainBotQuiescenceProbeScript(): String {
    return """
      set -eu
      self_pid="${'$'}$"
      self_ppid="${'$'}PPID"
      details=""
      process_matches() {
        pattern="${'$'}1"
        target_base="$(basename "${'$'}pattern")"
        printf '%s\n' "${'$'}ps_output" | awk -v pat="${'$'}pattern" -v target_base="${'$'}target_base" -v self_pid="${'$'}self_pid" -v self_ppid="${'$'}self_ppid" '
          function starts_with(value, prefix) { return index(value, prefix) == 1 }
          function next_is_boundary(value, prefix_len) {
            c = substr(value, prefix_len + 1, 1)
            return c == "" || c == " "
          }
          {
            pid = $1
            name = $2
            if (pid == self_pid || pid == self_ppid) {
              next
            }
            args = ""
            if (NF >= 3) {
              args = substr($0, index($0, $3))
            }
            if (name == target_base) {
              found = 1
              exit
            }
            if (
              args == pat ||
              (starts_with(args, pat) && next_is_boundary(args, length(pat))) ||
              (starts_with(args, "sh " pat) && next_is_boundary(args, length("sh " pat))) ||
              (starts_with(args, target_base) && next_is_boundary(args, length(target_base)))
            ) {
              found = 1
              exit
            }
          }
          END { exit(found ? 0 : 1) }
        ' >/dev/null 2>&1
      }
      for pid_file in \
        /data/local/pixel-stack/apps/train-bot/run/train-bot-service-loop.pid \
        /data/local/pixel-stack/apps/train-bot/run/train-bot.pid \
        /data/local/pixel-stack/apps/train-bot/run/train-web-tunnel-service-loop.pid \
        /data/local/pixel-stack/apps/train-bot/run/train-bot-cloudflared.pid; do
        if [ -s "${'$'}pid_file" ]; then
          pid="$(cat "${'$'}pid_file" 2>/dev/null || true)"
          if [ -n "${'$'}pid" ] && kill -0 "${'$'}pid" >/dev/null 2>&1; then
            details="${'$'}details pid_file=${'$'}pid_file:${'$'}pid;"
          fi
        fi
      done
      ps_output="$(ps -A -o PID=,NAME=,ARGS= 2>/dev/null || true)"
      for pattern in \
        /data/local/pixel-stack/apps/train-bot/bin/train-bot-service-loop \
        /data/local/pixel-stack/apps/train-bot/bin/train-web-tunnel-service-loop \
        /data/local/pixel-stack/apps/train-bot/bin/train-bot-launch \
        /data/local/pixel-stack/apps/train-bot/bin/cloudflared \
        /data/local/pixel-stack/apps/train-bot/bin/train-bot.current \
        /data/local/pixel-stack/apps/train-bot/bin/train-bot; do
        if process_matches "${'$'}pattern"; then
          details="${'$'}details process=${'$'}pattern;"
        fi
      done
      if [ -n "${'$'}details" ]; then
        printf '%s\n' "${'$'}details"
      else
        printf 'QUIESCENT\n'
      fi
    """.trimIndent()
  }

  private fun satiksmeBotQuiescenceProbeScript(): String {
    return """
      set -eu
      self_pid="${'$'}$"
      self_ppid="${'$'}PPID"
      details=""
      process_matches() {
        pattern="${'$'}1"
        target_base="$(basename "${'$'}pattern")"
        printf '%s\n' "${'$'}ps_output" | awk -v pat="${'$'}pattern" -v target_base="${'$'}target_base" -v self_pid="${'$'}self_pid" -v self_ppid="${'$'}self_ppid" '
          function starts_with(value, prefix) { return index(value, prefix) == 1 }
          function next_is_boundary(value, prefix_len) {
            c = substr(value, prefix_len + 1, 1)
            return c == "" || c == " "
          }
          {
            pid = $1
            name = $2
            if (pid == self_pid || pid == self_ppid) {
              next
            }
            args = ""
            if (NF >= 3) {
              args = substr($0, index($0, $3))
            }
            if (name == target_base) {
              found = 1
              exit
            }
            if (
              args == pat ||
              (starts_with(args, pat) && next_is_boundary(args, length(pat))) ||
              (starts_with(args, "sh " pat) && next_is_boundary(args, length("sh " pat))) ||
              (starts_with(args, target_base) && next_is_boundary(args, length(target_base)))
            ) {
              found = 1
              exit
            }
          }
          END { exit(found ? 0 : 1) }
        ' >/dev/null 2>&1
      }
      for pid_file in \
        /data/local/pixel-stack/apps/satiksme-bot/run/satiksme-bot-service-loop.pid \
        /data/local/pixel-stack/apps/satiksme-bot/run/satiksme-bot.pid \
        /data/local/pixel-stack/apps/satiksme-bot/run/satiksme-web-tunnel-service-loop.pid \
        /data/local/pixel-stack/apps/satiksme-bot/run/satiksme-bot-cloudflared.pid; do
        if [ -s "${'$'}pid_file" ]; then
          pid="$(cat "${'$'}pid_file" 2>/dev/null || true)"
          if [ -n "${'$'}pid" ] && kill -0 "${'$'}pid" >/dev/null 2>&1; then
            details="${'$'}details pid_file=${'$'}pid_file:${'$'}pid;"
          fi
        fi
      done
      ps_output="$(ps -A -o PID=,NAME=,ARGS= 2>/dev/null || true)"
      for pattern in \
        /data/local/pixel-stack/apps/satiksme-bot/bin/satiksme-bot-service-loop \
        /data/local/pixel-stack/apps/satiksme-bot/bin/satiksme-web-tunnel-service-loop \
        /data/local/pixel-stack/apps/satiksme-bot/bin/satiksme-bot-launch \
        /data/local/pixel-stack/apps/satiksme-bot/bin/cloudflared \
        /data/local/pixel-stack/apps/satiksme-bot/bin/satiksme-bot.current \
        /data/local/pixel-stack/apps/satiksme-bot/bin/satiksme-bot; do
        if process_matches "${'$'}pattern"; then
          details="${'$'}details process=${'$'}pattern;"
        fi
      done
      if [ -n "${'$'}details" ]; then
        printf '%s\n' "${'$'}details"
      else
        printf 'QUIESCENT\n'
      fi
    """.trimIndent()
  }

  private companion object {
    const val REQUIRED_SIGNATURE_SCHEMA = "none"
    val REQUIRED_BOOTSTRAP_ARTIFACT_IDS = listOf("adguardhome-rootfs", "dropbear-bundle", "tailscale-bundle")
    val OPTIONAL_BOOTSTRAP_ARTIFACT_IDS = listOf("train-bot-bundle", "satiksme-bot-bundle", "site-notifier-bundle")
    val SUPPORTED_COMPONENTS = setOf("dns", "ssh", "vpn", "train_bot", "satiksme_bot", "site_notifier", "ddns", "remote")
    const val ROOTFS_ARTIFACT_ID = "adguardhome-rootfs"
    const val BOOTSTRAP_HEALTH_WAIT_MILLIS = 90_000L
    const val BOOTSTRAP_HEALTH_RETRY_MILLIS = 3_000L
    const val HEALTHCHECK_WAIT_MILLIS = 90_000L
    const val HEALTHCHECK_RETRY_MILLIS = 3_000L
    const val QUIESCENCE_WAIT_MILLIS = 15_000L
    const val QUIESCENCE_RETRY_MILLIS = 1_000L
    const val RUNTIME_ENV_WRITE_MAX_ATTEMPTS = 3
    const val RUNTIME_ENV_WRITE_RETRY_MILLIS = 500L
    const val MAX_ERROR_FIELD_LENGTH = 600
  }

  private data class RedeploySpec(
    val requestedComponent: String,
    val runtimeConfigComponent: String,
    val runtimeAssetComponent: String,
    val releaseManifestComponent: String?,
    val releaseInstallComponent: String?,
    val runtimeAction: String,
    val runtimeActionComponent: String,
    val stopComponent: String = runtimeActionComponent,
    val requiresQuiescentInstall: Boolean = false,
    val quiescenceProbeScript: String = "",
    val staleCleanupCommand: String = "",
    val rollbackStrategy: RollbackStrategy = RollbackStrategy.NONE,
    val rollbackComponent: String = requestedComponent,
    val retryBudget: Int = 0,
    val healthGateComponents: List<String>,
    val targetComponents: Set<String>
  ) {
    val requiresReleaseManifest: Boolean
      get() = releaseManifestComponent != null && releaseInstallComponent != null
  }

  private enum class RollbackStrategy {
    NONE,
    PREVIOUS_CURRENT_RELEASE
  }

  private data class QuiescenceProbe(
    val quiescent: Boolean,
    val detail: String
  )

  private data class StopAndQuiesceOutcome(
    val success: Boolean,
    val failureCode: String = "",
    val detail: String = "",
    val cleanupAttempted: Boolean = false
  )

  private data class RollbackOutcome(
    val success: Boolean,
    val releaseId: String,
    val message: String,
    val healthSnapshot: HealthSnapshot? = null
  )
}
