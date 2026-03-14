package lv.jolkins.pixelorchestrator.supervisor

import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import lv.jolkins.pixelorchestrator.coreconfig.HealthSnapshot
import lv.jolkins.pixelorchestrator.coreconfig.ModuleRuntimeState
import lv.jolkins.pixelorchestrator.coreconfig.OperationEvent
import lv.jolkins.pixelorchestrator.coreconfig.ServiceRuntimeState
import lv.jolkins.pixelorchestrator.coreconfig.ServiceStatus
import lv.jolkins.pixelorchestrator.coreconfig.StackConfigV1
import lv.jolkins.pixelorchestrator.coreconfig.StackStateV1
import lv.jolkins.pixelorchestrator.coreconfig.StackStore
import lv.jolkins.pixelorchestrator.health.HealthScope
import lv.jolkins.pixelorchestrator.health.RuntimeHealthChecker

class SupervisorEngine(
  private val configProvider: () -> StackConfigV1,
  private val stateStore: StackStore,
  private val healthChecker: RuntimeHealthChecker,
  private val components: Map<String, ComponentController>
) : SupervisorControl {
  private val scope: CoroutineScope = CoroutineScope(SupervisorJob() + Dispatchers.Default)
  private var loopJob: Job? = null
  private val backoffs = mutableMapOf<String, BackoffPolicy>()
  private val unhealthyCounts = mutableMapOf<String, Int>()
  private var managementPendingSshRestart = false
  private var managementRecoveryCooldownUntilEpochSeconds = 0L

  override suspend fun startAll() {
    val config = configProvider()
    var state = stateStore.loadStateOrDefault().withSupervisorStatus(ServiceStatus.STARTING)

    components.values.forEach { controller ->
      val ok = controller.start()
      state = state
        .appendEvent(controller.name, "start", ok, "startAll")
        .markComponent(controller.name, if (ok) ServiceStatus.RUNNING else ServiceStatus.DEGRADED, if (ok) "" else "start failed")

      ensureBackoffPolicy(controller.name, config)
    }

    stateStore.saveState(state)

    loopJob?.cancel()
    loopJob = scope.launch {
      runLoop()
    }
  }

  override suspend fun stopAll() {
    loopJob?.cancel()
    var state = stateStore.loadStateOrDefault()
    components.values.forEach { controller ->
      val ok = controller.stop()
      state = state
        .appendEvent(controller.name, "stop", ok, "stopAll")
        .markComponent(controller.name, ServiceStatus.STOPPED, if (ok) "" else "stop failed", countAsRestart = false)
    }

    stateStore.saveState(state.withSupervisorStatus(ServiceStatus.STOPPED))
  }

  override suspend fun startComponent(component: String) {
    val controller = components[component] ?: return
    ensureBackoffPolicy(component)
    val ok = controller.start()
    val state = stateStore.loadStateOrDefault()
      .appendEvent(component, "start", ok, "manual start component")
      .markComponent(component, if (ok) ServiceStatus.RUNNING else ServiceStatus.DEGRADED, if (ok) "" else "start failed")
    stateStore.saveState(state)
  }

  override suspend fun stopComponent(component: String) {
    val controller = components[component] ?: return
    val ok = controller.stop()
    val state = stateStore.loadStateOrDefault()
      .appendEvent(component, "stop", ok, "manual stop component")
      .markComponent(component, ServiceStatus.STOPPED, if (ok) "" else "stop failed", countAsRestart = false)
    stateStore.saveState(state)
  }

  override suspend fun restart(component: String) {
    val controller = components[component] ?: return
    ensureBackoffPolicy(component)
    controller.stop()
    val ok = controller.start()

    val state = stateStore.loadStateOrDefault()
      .appendEvent(component, "restart", ok, "manual restart")
      .markComponent(component, if (ok) ServiceStatus.RUNNING else ServiceStatus.DEGRADED, if (ok) "" else "restart failed")

    stateStore.saveState(state)
  }

  override suspend fun runHealthCheck(scope: HealthScope): HealthSnapshot {
    val config = configProvider()
    var snapshot = healthChecker.check(config)
    val trainBotTunnelFailureCount =
      nextTrainBotTunnelFailureCount(
        trainBotHealthy = snapshot.trainBotHealthy,
        tunnelFailure = snapshot.trainBotTunnelFailure()
      )
    val satiksmeBotTunnelFailureCount =
      nextSatiksmeBotTunnelFailureCount(
        satiksmeBotHealthy = snapshot.satiksmeBotHealthy,
        tunnelFailure = snapshot.satiksmeBotTunnelFailure()
      )
    snapshot = snapshot
      .withTrainBotTunnelFailureCount(trainBotTunnelFailureCount)
      .withSatiksmeBotTunnelFailureCount(satiksmeBotTunnelFailureCount)
    val state = stateStore.loadStateOrDefault()
      .withModuleHealth(snapshot)
      .copy(lastHealthSnapshot = snapshot)
      .appendEvent("health", "check", true, "scope=$scope")

    stateStore.saveState(state)
    return snapshot
  }

  override suspend fun syncDdnsNow() {
    val controller = components["ddns"] ?: return
    val ok = controller.start()
    val state = stateStore.loadStateOrDefault()
      .appendEvent("ddns", "sync_now", ok, "manual")
      .markComponent("ddns", if (ok) ServiceStatus.RUNNING else ServiceStatus.DEGRADED, if (ok) "" else "sync failed")

    stateStore.saveState(state)
  }

  private suspend fun runLoop() {
    while (scope.isActive) {
      val config = configProvider()
      var snapshot = healthChecker.check(config)
      val remoteEnabled = config.remote.dohEnabled || config.remote.dotEnabled
      val remoteEscalationEnabled =
        remoteEnabled && config.supervision.enforceRemoteListeners && config.remote.watchdogEscalateRuntimeRestart
      val dnsRestartHealthy = snapshot.dnsHealthy && (!remoteEscalationEnabled || snapshot.remoteHealthy)
      val trainBotTunnelFailureCount =
        nextTrainBotTunnelFailureCount(
          trainBotHealthy = snapshot.trainBotHealthy,
          tunnelFailure = snapshot.trainBotTunnelFailure()
        )
      val satiksmeBotTunnelFailureCount =
        nextSatiksmeBotTunnelFailureCount(
          satiksmeBotHealthy = snapshot.satiksmeBotHealthy,
          tunnelFailure = snapshot.satiksmeBotTunnelFailure()
        )
      snapshot = snapshot
        .withTrainBotTunnelFailureCount(trainBotTunnelFailureCount)
        .withSatiksmeBotTunnelFailureCount(satiksmeBotTunnelFailureCount)
      var state = stateStore.loadStateOrDefault()
        .withModuleHealth(snapshot)
        .copy(lastHealthSnapshot = snapshot)
        .withSupervisorStatus(
          if (snapshot.supervisorHealthy) ServiceStatus.RUNNING else ServiceStatus.DEGRADED
        )

      val dnsOutcome = restartDnsIfUnhealthy(snapshot, state, config, remoteEscalationEnabled)
      state = dnsOutcome.state

      val vpnRequired = config.vpn.enabled || (config.modules["vpn"]?.enabled ?: false)
      val managementOutcome = recoverManagementPath(snapshot, state, config, vpnRequired)
      state = managementOutcome.state

      val sshOutcome =
        if (managementEnabled(snapshot)) RestartOutcome(state) else restartIfUnhealthy("ssh", snapshot.sshHealthy, state)
      state = sshOutcome.state

      val vpnOutcome =
        if (managementEnabled(snapshot)) RestartOutcome(state) else restartIfUnhealthy("vpn", if (vpnRequired) snapshot.vpnHealthy else true, state)
      state = vpnOutcome.state

      val trainOutcome = restartTrainBotIfUnhealthy(snapshot, state, config)
      state = trainOutcome.state

      val satiksmeOutcome = restartSatiksmeBotIfUnhealthy(snapshot, state, config)
      state = satiksmeOutcome.state

      val notifierOutcome = restartIfUnhealthy("site_notifier", snapshot.siteNotifierHealthy, state)
      state = notifierOutcome.state

      state = syncDdnsIfDue(state, config, snapshot)
      state = observeRemoteHealth(state, snapshot.remoteHealthy)
      state = observeManagementHealth(state, snapshot)
      stateStore.saveState(state)

      val restartDelayMillis = listOf(
        dnsOutcome.delayMillis,
        managementOutcome.delayMillis,
        sshOutcome.delayMillis,
        vpnOutcome.delayMillis,
        trainOutcome.delayMillis,
        satiksmeOutcome.delayMillis,
        notifierOutcome.delayMillis
      ).maxOrNull() ?: 0L
      if (restartDelayMillis > 0L) {
        delay(restartDelayMillis)
      }
      delay((config.supervision.healthPollSeconds.coerceAtLeast(1) * 1000L))
    }
  }

  private suspend fun restartTrainBotIfUnhealthy(
    snapshot: HealthSnapshot,
    state: StackStateV1,
    config: StackConfigV1
  ): RestartOutcome {
    if (snapshot.trainBotHealthy) {
      unhealthyCounts["train_bot"] = 0
      backoffs["train_bot"]?.reset()
      return RestartOutcome(state)
    }

    val failureCount = unhealthyCounts["train_bot"] ?: 0
    if (snapshot.trainBotTunnelFailure()) {
      val threshold = config.supervision.unhealthyFails.coerceAtLeast(1)
      if (failureCount < threshold) {
        return RestartOutcome(
          state = state
            .appendEvent("train_bot", "health_unhealthy", false, "tunnel/public probe failed count=$failureCount threshold=$threshold")
            .markComponent(
              "train_bot",
              ServiceStatus.DEGRADED,
              "tunnel/public probe failed ($failureCount/$threshold)",
              countAsRestart = false
            )
        )
      }
    }

    return restartIfUnhealthy("train_bot", false, state)
  }

  private suspend fun restartSatiksmeBotIfUnhealthy(
    snapshot: HealthSnapshot,
    state: StackStateV1,
    config: StackConfigV1
  ): RestartOutcome {
    if (snapshot.satiksmeBotHealthy) {
      unhealthyCounts["satiksme_bot"] = 0
      backoffs["satiksme_bot"]?.reset()
      return RestartOutcome(state)
    }

    val failureCount = unhealthyCounts["satiksme_bot"] ?: 0
    if (snapshot.satiksmeBotTunnelFailure()) {
      val threshold = config.supervision.unhealthyFails.coerceAtLeast(1)
      if (failureCount < threshold) {
        return RestartOutcome(
          state = state
            .appendEvent("satiksme_bot", "health_unhealthy", false, "tunnel/public probe failed count=$failureCount threshold=$threshold")
            .markComponent(
              "satiksme_bot",
              ServiceStatus.DEGRADED,
              "tunnel/public probe failed ($failureCount/$threshold)",
              countAsRestart = false
            )
        )
      }
    }

    return restartIfUnhealthy("satiksme_bot", false, state)
  }

  private suspend fun recoverManagementPath(
    snapshot: HealthSnapshot,
    state: StackStateV1,
    config: StackConfigV1,
    vpnRequired: Boolean
  ): RestartOutcome {
    if (!managementEnabled(snapshot)) {
      unhealthyCounts["management"] = 0
      managementPendingSshRestart = false
      managementRecoveryCooldownUntilEpochSeconds = 0L
      return RestartOutcome(state)
    }

    val reason = managementReason(snapshot)
    val nowEpoch = System.currentTimeMillis() / 1000

    if (managementPendingSshRestart && snapshot.vpnHealthy) {
      managementPendingSshRestart = false
      unhealthyCounts["management"] = 0
      managementRecoveryCooldownUntilEpochSeconds = nowEpoch + config.supervision.managementRecoveryCooldownSeconds.coerceAtLeast(0)
      return restartComponentForManagement(
        target = "ssh",
        state = state,
        reason = reason,
        detail = "coordinated recovery step=ssh"
      )
    }

    if (snapshot.managementHealthy) {
      unhealthyCounts["management"] = 0
      managementRecoveryCooldownUntilEpochSeconds = 0L
      return RestartOutcome(state)
    }

    if (!vpnRequired || !snapshot.vpnHealthy) {
      managementPendingSshRestart = false
      unhealthyCounts["management"] = 0
      return restartComponentForManagement(
        target = "vpn",
        state = state,
        reason = reason,
        detail = "vpn-first recovery"
      )
    }

    if (!snapshot.sshHealthy) {
      managementPendingSshRestart = false
      unhealthyCounts["management"] = 0
      return restartComponentForManagement(
        target = "ssh",
        state = state,
        reason = reason,
        detail = "ssh recovery"
      )
    }

    if (nowEpoch < managementRecoveryCooldownUntilEpochSeconds) {
      return RestartOutcome(
        state = state.appendEvent(
          "management",
          "health_unhealthy",
          false,
          "reason=$reason cooldown_remaining=${managementRecoveryCooldownUntilEpochSeconds - nowEpoch}s"
        )
      )
    }

    val nextFailureCount = (unhealthyCounts["management"] ?: 0) + 1
    unhealthyCounts["management"] = nextFailureCount
    val threshold = config.supervision.managementUnhealthyFails.coerceAtLeast(1)
    if (nextFailureCount < threshold) {
      return RestartOutcome(
        state = state
          .appendEvent("management", "health_unhealthy", false, "reason=$reason count=$nextFailureCount threshold=$threshold")
          .markComponent(
            "management",
            ServiceStatus.DEGRADED,
            "$reason ($nextFailureCount/$threshold)",
            countAsRestart = false
          )
      )
    }

    unhealthyCounts["management"] = 0
    managementPendingSshRestart = true
    return restartComponentForManagement(
      target = "vpn",
      state = state,
      reason = reason,
      detail = "coordinated recovery step=vpn"
    )
  }

  private suspend fun restartDnsIfUnhealthy(
    snapshot: HealthSnapshot,
    state: StackStateV1,
    config: StackConfigV1,
    remoteEscalationEnabled: Boolean
  ): RestartOutcome {
    val remotePublicFailure = remoteEscalationEnabled && snapshot.dnsHealthy && remotePublicContractFailed(snapshot)
    if (!remotePublicFailure) {
      unhealthyCounts["dns_remote"] = 0
      if (snapshot.dnsHealthy) {
        backoffs["dns"]?.reset()
        return RestartOutcome(state)
      }
      return restartIfUnhealthy("dns", false, state)
    }

    val nextFailureCount = (unhealthyCounts["dns_remote"] ?: 0) + 1
    unhealthyCounts["dns_remote"] = nextFailureCount
    val threshold = config.supervision.unhealthyFails.coerceAtLeast(1)
    if (nextFailureCount < threshold) {
      return RestartOutcome(
        state = state
          .appendEvent("dns", "health_unhealthy", false, "remote/public probe failed count=$nextFailureCount threshold=$threshold")
          .markComponent(
            "dns",
            ServiceStatus.DEGRADED,
            "remote/public probe failed ($nextFailureCount/$threshold)",
            countAsRestart = false
          )
      )
    }

    unhealthyCounts["dns_remote"] = 0
    return restartIfUnhealthy("dns", false, state)
  }

  private fun remotePublicContractFailed(snapshot: HealthSnapshot): Boolean {
    val evidence = snapshot.evidence
    if (evidence["remote_public_probe_available"] != "true") return false

    val publicRootCode = evidence["remote_public_root_code"].orEmpty()
    val publicTokenizedCode = evidence["remote_public_doh_tokenized_code"].orEmpty()
    val publicBareCode = evidence["remote_public_doh_bare_code"].orEmpty()
    val publicIdentityCode = evidence["remote_public_identity_inject_code"].orEmpty()
    val dohEndpointMode = evidence["doh_endpoint_mode"].orEmpty()
    val rootHealthy = evidence["remote_public_root_healthy"] == "true"
    val dohContractHealthy = evidence["remote_public_doh_contract"] == "true"
    val identityRequired = evidence["identity_frontend_required"] == "true"
    val publicIdentityHealthy = evidence["remote_public_identity_frontend_healthy"] == "true"

    val rootInconclusive = publicProbeInconclusive(publicRootCode)
    val dohInconclusive = when (dohEndpointMode) {
      "tokenized", "dual" -> publicProbeInconclusive(publicTokenizedCode) || publicProbeInconclusive(publicBareCode)
      "native" -> publicProbeInconclusive(publicBareCode)
      else -> publicProbeInconclusive(publicTokenizedCode) && publicProbeInconclusive(publicBareCode)
    }
    val identityInconclusive = publicProbeInconclusive(publicIdentityCode)

    return (!rootInconclusive && !rootHealthy) ||
      (!dohInconclusive && !dohContractHealthy) ||
      (identityRequired && !identityInconclusive && !publicIdentityHealthy)
  }

  private fun publicProbeInconclusive(code: String): Boolean = code == "000"

  private fun nextTrainBotTunnelFailureCount(trainBotHealthy: Boolean, tunnelFailure: Boolean): Int {
    if (trainBotHealthy || !tunnelFailure) {
      unhealthyCounts["train_bot"] = 0
      return 0
    }
    val next = (unhealthyCounts["train_bot"] ?: 0) + 1
    unhealthyCounts["train_bot"] = next
    return next
  }

  private fun nextSatiksmeBotTunnelFailureCount(satiksmeBotHealthy: Boolean, tunnelFailure: Boolean): Int {
    if (satiksmeBotHealthy || !tunnelFailure) {
      unhealthyCounts["satiksme_bot"] = 0
      return 0
    }
    val next = (unhealthyCounts["satiksme_bot"] ?: 0) + 1
    unhealthyCounts["satiksme_bot"] = next
    return next
  }

  private suspend fun restartIfUnhealthy(name: String, healthy: Boolean, state: StackStateV1): RestartOutcome {
    val controller = components[name] ?: return RestartOutcome(state)
    if (healthy) {
      backoffs[name]?.reset()
      return RestartOutcome(state)
    }

    val policy = backoffs[name] ?: return RestartOutcome(state)
    val decision = policy.recordRestart()

    if (decision.crashLoop) {
      return RestartOutcome(
        state = state
          .markComponent(name, ServiceStatus.CRASH_LOOP, "too many rapid restarts", countAsRestart = false)
          .appendEvent(name, "crash_loop", false, "rapid=${decision.rapidCount}"),
        delayMillis = decision.sleepSeconds * 1000L
      )
    }

    val ok = controller.start()
    return RestartOutcome(
      state = state
        .appendEvent(name, "auto_restart", ok, "delay=${decision.sleepSeconds}s rapid=${decision.rapidCount}")
        .markComponent(name, if (ok) ServiceStatus.RUNNING else ServiceStatus.DEGRADED, if (ok) "" else "restart failed"),
      delayMillis = decision.sleepSeconds * 1000L
    )
  }

  private suspend fun restartComponentForManagement(
    target: String,
    state: StackStateV1,
    reason: String,
    detail: String
  ): RestartOutcome {
    val controller = components[target] ?: return RestartOutcome(state)
    controller.stop()
    val ok = controller.start()
    return RestartOutcome(
      state = state
        .appendEvent("management", "auto_recovery", ok, "target=$target reason=$reason detail=$detail")
        .markComponent(target, if (ok) ServiceStatus.RUNNING else ServiceStatus.DEGRADED, if (ok) "" else "restart failed"),
      delayMillis = 0L
    )
  }

  private suspend fun syncDdnsIfDue(
    state: StackStateV1,
    config: StackConfigV1,
    snapshot: HealthSnapshot
  ): StackStateV1 {
    if (!config.ddns.enabled) return state
    val controller = components["ddns"] ?: return state

    val now = System.currentTimeMillis() / 1000
    val intervalSeconds = config.ddns.intervalSeconds.coerceAtLeast(1)
    val ddnsState = state.services["ddns"] ?: ServiceRuntimeState()
    val ageSeconds = if (ddnsState.lastStartedEpochSeconds <= 0L) Long.MAX_VALUE else now - ddnsState.lastStartedEpochSeconds
    val due = !snapshot.ddnsHealthy || ageSeconds >= intervalSeconds
    if (!due) return state

    val ok = controller.start()
    val reason = when {
      !snapshot.ddnsHealthy -> "unhealthy"
      ddnsState.lastStartedEpochSeconds <= 0L -> "first_run"
      else -> "interval_elapsed"
    }
    return state
      .appendEvent("ddns", "auto_sync", ok, "reason=${reason} age=${if (ageSeconds == Long.MAX_VALUE) "never" else ageSeconds}s")
      .markComponent("ddns", if (ok) ServiceStatus.RUNNING else ServiceStatus.DEGRADED, if (ok) "" else "sync failed")
  }

  private fun observeRemoteHealth(state: StackStateV1, healthy: Boolean): StackStateV1 {
    val current = state.services["remote"] ?: ServiceRuntimeState()
    if (healthy) {
      backoffs["remote"]?.reset()
      return if (current.status == ServiceStatus.DEGRADED || current.status == ServiceStatus.CRASH_LOOP) {
        state
          .markComponent("remote", ServiceStatus.RUNNING, "", countAsRestart = false)
          .appendEvent("remote", "health_recovered", true, "watchdog owned")
      } else {
        state
      }
    }

    return if (current.status != ServiceStatus.DEGRADED) {
      state
        .markComponent("remote", ServiceStatus.DEGRADED, "remote healthcheck failed", countAsRestart = false)
        .appendEvent("remote", "health_unhealthy", false, "watchdog owned")
    } else {
      state
    }
  }

  private fun observeManagementHealth(state: StackStateV1, snapshot: HealthSnapshot): StackStateV1 {
    if (!managementEnabled(snapshot)) {
      return state
    }

    val healthy = snapshot.managementHealthy
    val reason = managementReason(snapshot)
    val current = state.services["management"] ?: ServiceRuntimeState()
    if (healthy) {
      return if (current.status == ServiceStatus.DEGRADED || current.status == ServiceStatus.CRASH_LOOP) {
        state
          .markComponent("management", ServiceStatus.RUNNING, "", countAsRestart = false)
          .appendEvent("management", "health_recovered", true, "reason=$reason")
      } else {
        state
      }
    }

    return if (current.status != ServiceStatus.DEGRADED) {
      state
        .markComponent("management", ServiceStatus.DEGRADED, reason, countAsRestart = false)
        .appendEvent("management", "health_unhealthy", false, "reason=$reason")
    } else {
      state
    }
  }

  private fun managementEnabled(snapshot: HealthSnapshot): Boolean {
    return snapshot.evidence["management_enabled"] == "true"
  }

  private fun managementReason(snapshot: HealthSnapshot): String {
    return snapshot.evidence["management_reason"].orEmpty().ifBlank { "unknown" }
  }

  private fun StackStateV1.markComponent(
    name: String,
    status: ServiceStatus,
    failure: String,
    countAsRestart: Boolean = true
  ): StackStateV1 {
    val now = System.currentTimeMillis() / 1000
    val current = services[name] ?: ServiceRuntimeState()
    val updated = current.copy(
      status = status,
      restartCount = if (countAsRestart && status == ServiceStatus.RUNNING && current.status != ServiceStatus.RUNNING) {
        current.restartCount + 1
      } else {
        current.restartCount
      },
      lastFailureReason = failure,
      lastStartedEpochSeconds = if (status == ServiceStatus.RUNNING) now else current.lastStartedEpochSeconds,
      lastHealthyEpochSeconds = if (status == ServiceStatus.RUNNING) now else current.lastHealthyEpochSeconds
    )

    return copy(services = services + (name to updated))
  }

  private fun StackStateV1.appendEvent(component: String, action: String, success: Boolean, details: String): StackStateV1 {
    val next = operationLog
      .plus(
        OperationEvent(
          epochSeconds = System.currentTimeMillis() / 1000,
          component = component,
          action = action,
          success = success,
          details = details
        )
      )
      .takeLast(100)

    return copy(operationLog = next)
  }

  private fun StackStateV1.withSupervisorStatus(status: ServiceStatus): StackStateV1 {
    val now = System.currentTimeMillis() / 1000
    val supervisor = services["supervisor"] ?: ServiceRuntimeState()
    return copy(
      services = services + ("supervisor" to supervisor.copy(
        status = status,
        lastStartedEpochSeconds = if (status == ServiceStatus.RUNNING) now else supervisor.lastStartedEpochSeconds,
        lastHealthyEpochSeconds = if (status == ServiceStatus.RUNNING) now else supervisor.lastHealthyEpochSeconds
      )),
      lastSuccessfulBootEpochSeconds = if (status == ServiceStatus.RUNNING) now else lastSuccessfulBootEpochSeconds
    )
  }

  private fun StackStateV1.withModuleHealth(snapshot: HealthSnapshot): StackStateV1 {
    val now = System.currentTimeMillis() / 1000
    val merged = moduleState.toMutableMap()
    snapshot.moduleHealth.forEach { (moduleId, moduleHealth) ->
      merged[moduleId] = ModuleRuntimeState(
        status = moduleHealth.status,
        healthy = moduleHealth.healthy,
        lastUpdatedEpochSeconds = now,
        details = moduleHealth.details
      )
    }
    return copy(moduleState = merged)
  }

  private data class RestartOutcome(
    val state: StackStateV1,
    val delayMillis: Long = 0L
  )

  private fun ensureBackoffPolicy(name: String, config: StackConfigV1 = configProvider()) {
    if (!backoffs.containsKey(name)) {
      val (initialSeconds, maxSeconds, rapidWindowSeconds, maxRapidRestarts) = when (name) {
        "train_bot" -> listOf(
          config.trainBot.backoffInitialSeconds,
          config.trainBot.backoffMaxSeconds,
          config.trainBot.rapidWindowSeconds,
          config.trainBot.maxRapidRestarts
        )
        "satiksme_bot" -> listOf(
          config.satiksmeBot.backoffInitialSeconds,
          config.satiksmeBot.backoffMaxSeconds,
          config.satiksmeBot.rapidWindowSeconds,
          config.satiksmeBot.maxRapidRestarts
        )
        "ssh" -> listOf(
          config.ssh.backoffInitialSeconds,
          config.ssh.backoffMaxSeconds,
          config.ssh.rapidWindowSeconds,
          config.ssh.maxRapidRestarts
        )
        "vpn" -> listOf(
          config.vpn.backoffInitialSeconds,
          config.vpn.backoffMaxSeconds,
          config.vpn.rapidWindowSeconds,
          config.vpn.maxRapidRestarts
        )
        "site_notifier" -> listOf(
          config.siteNotifier.backoffInitialSeconds,
          config.siteNotifier.backoffMaxSeconds,
          config.siteNotifier.rapidWindowSeconds,
          config.siteNotifier.maxRapidRestarts
        )
        else -> listOf(
          config.supervision.backoffInitialSeconds,
          config.supervision.backoffMaxSeconds,
          config.supervision.rapidWindowSeconds,
          config.supervision.maxRapidRestarts
        )
      }
      backoffs[name] = BackoffPolicy(
        initialSeconds = initialSeconds,
        maxSeconds = maxSeconds,
        rapidWindowSeconds = rapidWindowSeconds,
        maxRapidRestarts = maxRapidRestarts
      )
    }
  }

  private fun HealthSnapshot.trainBotTunnelFailure(): Boolean {
    return evidence["train_bot_tunnel_enabled"] == "true" && evidence["train_bot_tunnel_healthy"] == "false"
  }

  private fun HealthSnapshot.satiksmeBotTunnelFailure(): Boolean {
    if (satiksmeBotHealthy) return false
    return when (evidence["satiksme_bot_failure_reason"].orEmpty()) {
      "tunnel_supervisor_missing",
      "tunnel_pid_missing",
      "tunnel_probe_unavailable",
      "public_root_failed",
      "public_app_failed" -> true
      else -> false
    }
  }

  private fun HealthSnapshot.withTrainBotTunnelFailureCount(count: Int): HealthSnapshot {
    val module = moduleHealth["train_bot"]
    val updatedModuleHealth =
      if (module == null) {
        moduleHealth
      } else {
        moduleHealth + ("train_bot" to module.copy(details = module.details + ("tunnel_failure_count" to count.toString())))
      }
    return copy(
      moduleHealth = updatedModuleHealth,
      evidence = evidence + ("train_bot_tunnel_failure_count" to count.toString())
    )
  }

  private fun HealthSnapshot.withSatiksmeBotTunnelFailureCount(count: Int): HealthSnapshot {
    val module = moduleHealth["satiksme_bot"]
    val updatedModuleHealth =
      if (module == null) {
        moduleHealth
      } else {
        moduleHealth + ("satiksme_bot" to module.copy(details = module.details + ("tunnel_failure_count" to count.toString())))
      }
    return copy(
      moduleHealth = updatedModuleHealth,
      evidence = evidence + ("satiksme_bot_tunnel_failure_count" to count.toString())
    )
  }
}
