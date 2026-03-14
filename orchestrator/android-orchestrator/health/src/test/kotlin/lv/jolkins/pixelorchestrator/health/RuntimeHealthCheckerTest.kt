package lv.jolkins.pixelorchestrator.health

import kotlin.test.assertEquals
import kotlin.test.assertFalse
import kotlin.test.assertTrue
import kotlinx.coroutines.runBlocking
import lv.jolkins.pixelorchestrator.coreconfig.StackConfigV1
import org.junit.Test

class RuntimeHealthCheckerTest {

  @Test
  fun buildProbeCommandIsShellParsable() {
    val checker = RuntimeHealthChecker(CommandRunner { CommandResult(ok = true, stdout = "", stderr = "") })
    val method = RuntimeHealthChecker::class.java.getDeclaredMethod("buildProbeCommand", StackConfigV1::class.java)
    method.isAccessible = true

    val command = method.invoke(checker, StackConfigV1()) as String
    val process = ProcessBuilder("/bin/sh", "-n", "-c", command).start()
    val stderr = process.errorStream.bufferedReader().use { it.readText() }

    assertEquals(0, process.waitFor(), stderr.ifBlank { "shell parse failed" })
  }

  @Test
  fun usesSingleBatchedProbeAndSynthesizesHealthySnapshot() {
    var calls = 0
    val runner = CommandRunner { cmd ->
      calls += 1
      val trainPidFallbackSnippet =
        """ps -A 2>/dev/null | awk '((${'$'}NF=="train-bot") || index(${'$'}NF,"train-bot.")==1) {print ${'$'}2; exit}'"""
      val trainTunnelSupervisorFallbackSnippet =
        """scan_pid_by_target /data/local/pixel-stack/apps/train-bot/bin/train-web-tunnel-service-loop"""
      val trainTunnelPidFallbackSnippet =
        """scan_pid_by_target /data/local/pixel-stack/apps/train-bot/bin/cloudflared"""
      val satiksmeTunnelSupervisorFallbackSnippet =
        """scan_pid_by_target /data/local/pixel-stack/apps/satiksme-bot/bin/satiksme-web-tunnel-service-loop"""
      val satiksmeTunnelPidFallbackSnippet =
        """scan_pid_by_target /data/local/pixel-stack/apps/satiksme-bot/bin/cloudflared"""
      assertTrue(cmd.contains("__PIXEL_HEALTH_ID_U__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_LISTENERS__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_DDNS_EPOCH__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_TRAIN_BOT_PID__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_TRAIN_BOT_TUNNEL_ENABLED__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_TRAIN_BOT_TUNNEL_PID__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_TRAIN_BOT_TUNNEL_PUBLIC_BASE_URL__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_TRAIN_BOT_PUBLIC_ROOT_CODE__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_TRAIN_BOT_PUBLIC_APP_CODE__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_TRAIN_BOT_TUNNEL_PROBE_AVAILABLE__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_TRAIN_BOT_SCHEDULE_REQUIRED__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_TRAIN_BOT_SCHEDULE_FRESH__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_TRAIN_BOT_SCHEDULE_SERVICE_DATE__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_TRAIN_BOT_SCHEDULE_ROWS__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_SATIKSME_BOT_PID__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_SATIKSME_BOT_TUNNEL_ENABLED__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_SATIKSME_BOT_TUNNEL_PID__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_SATIKSME_BOT_PUBLIC_ROOT_CODE__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_SATIKSME_BOT_PUBLIC_APP_CODE__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_SATIKSME_BOT_TUNNEL_PROBE_AVAILABLE__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_SATIKSME_BOT_HEARTBEAT__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_SITE_NOTIFIER_PID__"))
      assertTrue(cmd.contains("train-bot.pid"))
      assertTrue(cmd.contains("satiksme-bot.pid"))
      assertTrue(cmd.contains("site-notifier.pid"))
      assertTrue(cmd.contains(trainPidFallbackSnippet))
      assertTrue(cmd.contains(trainTunnelSupervisorFallbackSnippet))
      assertTrue(cmd.contains(trainTunnelPidFallbackSnippet))
      assertTrue(cmd.contains(satiksmeTunnelSupervisorFallbackSnippet))
      assertTrue(cmd.contains(satiksmeTunnelPidFallbackSnippet))
      assertFalse(cmd.contains("app.py daemon"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_VPN_HEALTH__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_VPN_ENABLED_EFFECTIVE__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_VPN_TAILSCALED_LIVE__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_VPN_TAILSCALED_SOCK__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_VPN_TAILNET_IPV4__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_VPN_GUARD_CHAIN_IPV4__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_VPN_GUARD_CHAIN_IPV6__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_REMOTE_DOH_TOKENIZED_CODE__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_REMOTE_DOH_BARE_CODE__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_REMOTE_IDENTITY_INJECT_CODE__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_REMOTE_PUBLIC_BASE_URL__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_REMOTE_PUBLIC_ROOT_CODE__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_REMOTE_PUBLIC_PROBE_AVAILABLE__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_REMOTE_PUBLIC_DOH_TOKENIZED_CODE__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_REMOTE_PUBLIC_DOH_BARE_CODE__"))
      assertTrue(cmd.contains("__PIXEL_HEALTH_REMOTE_PUBLIC_IDENTITY_INJECT_CODE__"))

      CommandResult(
        ok = true,
        stdout = probeOutput(
          idU = "0",
          listeners = "LISTEN 0 128 0.0.0.0:53\nLISTEN 0 128 0.0.0.0:2222\nLISTEN 0 128 0.0.0.0:443\nLISTEN 0 128 0.0.0.0:853\n",
          ddnsEpoch = (System.currentTimeMillis() / 1000).toString(),
          trainBotPid = "1234",
          trainBotHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          siteNotifierPid = "2234",
          siteNotifierHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          vpnHealth = "1"
        ),
        stderr = ""
      )
    }

    val checker = RuntimeHealthChecker(runner)
    val config = StackConfigV1().copy(remote = StackConfigV1().remote.copy(dohEnabled = true, dotEnabled = true))
    val snapshot = runBlocking { checker.check(config) }

    assertEquals(1, calls)
    assertTrue(snapshot.rootGranted)
    assertTrue(snapshot.dnsHealthy)
    assertTrue(snapshot.sshHealthy)
    assertTrue(snapshot.vpnHealthy)
    assertTrue(snapshot.trainBotHealthy)
    assertTrue(snapshot.satiksmeBotHealthy)
    assertTrue(snapshot.siteNotifierHealthy)
    assertTrue(snapshot.remoteHealthy)
    assertTrue(snapshot.ddnsHealthy)
    assertTrue(snapshot.supervisorHealthy)
    assertEquals("true", snapshot.evidence["listeners_ok"])
    assertEquals("false", snapshot.evidence["train_bot_schedule_required"])
    assertEquals("true", snapshot.evidence["train_bot_schedule_fresh"])
    assertEquals("100.64.0.10", snapshot.evidence["vpn_tailnet_ipv4"])
  }

  @Test
  fun marksSnapshotDegradedWhenProbeOutputCannotBeParsed() {
    val runner = CommandRunner { _ ->
      CommandResult(ok = true, stdout = "unexpected output", stderr = "")
    }

    val checker = RuntimeHealthChecker(runner)
    val config = StackConfigV1().copy(remote = StackConfigV1().remote.copy(dohEnabled = true, dotEnabled = true))
    val snapshot = runBlocking { checker.check(config) }

    assertFalse(snapshot.rootGranted)
    assertFalse(snapshot.dnsHealthy)
    assertFalse(snapshot.sshHealthy)
    assertFalse(snapshot.trainBotHealthy)
    assertFalse(snapshot.siteNotifierHealthy)
    assertFalse(snapshot.remoteHealthy)
    assertFalse(snapshot.ddnsHealthy)
    assertFalse(snapshot.supervisorHealthy)
    assertEquals("false", snapshot.evidence["listeners_ok"])
  }

  @Test
  fun marksSupervisorUnhealthyWhenRemoteIsRequiredButRemotePortsMissing() {
    val runner = CommandRunner {
      CommandResult(
        ok = true,
        stdout = probeOutput(
          idU = "0",
          listeners = "LISTEN 0 128 0.0.0.0:53\nLISTEN 0 128 0.0.0.0:2222\n",
          ddnsEpoch = (System.currentTimeMillis() / 1000).toString(),
          trainBotPid = "1234",
          trainBotHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          siteNotifierPid = "2234",
          siteNotifierHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          vpnHealth = "1"
        ),
        stderr = ""
      )
    }

    val checker = RuntimeHealthChecker(runner)
    val config = StackConfigV1().copy(
      remote = StackConfigV1().remote.copy(dohEnabled = true, dotEnabled = false, watchdogEscalateRuntimeRestart = true)
    )
    val snapshot = runBlocking { checker.check(config) }

    assertTrue(snapshot.dnsHealthy)
    assertTrue(snapshot.sshHealthy)
    assertTrue(snapshot.vpnHealthy)
    assertFalse(snapshot.remoteHealthy)
    assertFalse(snapshot.supervisorHealthy)
  }

  @Test
  fun keepsSupervisorHealthyWhenRemoteListenerEnforcementIsDisabled() {
    val runner = CommandRunner {
      CommandResult(
        ok = true,
        stdout = probeOutput(
          idU = "0",
          listeners = "LISTEN 0 128 0.0.0.0:53\nLISTEN 0 128 0.0.0.0:2222\n",
          ddnsEpoch = (System.currentTimeMillis() / 1000).toString(),
          trainBotPid = "1234",
          trainBotHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          siteNotifierPid = "2234",
          siteNotifierHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          vpnHealth = "1"
        ),
        stderr = ""
      )
    }

    val checker = RuntimeHealthChecker(runner)
    val config = StackConfigV1().copy(remote = StackConfigV1().remote.copy(dohEnabled = true, dotEnabled = false))
    val snapshot = runBlocking { checker.check(config) }

    assertTrue(snapshot.supervisorHealthy)
    assertTrue(snapshot.remoteHealthy)
    assertEquals("false", snapshot.evidence["remote_health_enforced"])
  }

  @Test
  fun reportsSatiksmeFailureReasonForTunnelSupervisorFailures() {
    val nowEpoch = System.currentTimeMillis() / 1000
    val runner = CommandRunner {
      CommandResult(
        ok = true,
        stdout = probeOutput(
          idU = "0",
          listeners = "LISTEN 0 128 0.0.0.0:53\nLISTEN 0 128 0.0.0.0:2222\n",
          ddnsEpoch = nowEpoch.toString(),
          trainBotPid = "1234",
          trainBotHeartbeat = nowEpoch.toString(),
          satiksmeBotPid = "3234",
          satiksmeBotTunnelEnabled = "1",
          satiksmeBotTunnelSupervisorPid = "",
          satiksmeBotTunnelPid = "5555",
          satiksmeBotTunnelPublicBaseUrl = "https://satiksme-bot.example.com",
          satiksmeBotTunnelProbeAvailable = "1",
          satiksmeBotHeartbeat = nowEpoch.toString(),
          siteNotifierPid = "2234",
          siteNotifierHeartbeat = nowEpoch.toString(),
          vpnHealth = "1"
        ),
        stderr = ""
      )
    }

    val checker = RuntimeHealthChecker(runner)
    val snapshot = runBlocking { checker.check(StackConfigV1()) }

    assertFalse(snapshot.satiksmeBotHealthy)
    assertEquals("tunnel_supervisor_missing", snapshot.evidence["satiksme_bot_failure_reason"])
    assertEquals("tunnel_supervisor_missing", snapshot.moduleHealth["satiksme_bot"]?.details?.get("failure_reason"))
  }

  @Test
  fun reportsSatiksmeFailureReasonForStaleHeartbeat() {
    val nowEpoch = System.currentTimeMillis() / 1000
    val runner = CommandRunner {
      CommandResult(
        ok = true,
        stdout = probeOutput(
          idU = "0",
          listeners = "LISTEN 0 128 0.0.0.0:53\nLISTEN 0 128 0.0.0.0:2222\n",
          ddnsEpoch = nowEpoch.toString(),
          trainBotPid = "1234",
          trainBotHeartbeat = nowEpoch.toString(),
          satiksmeBotPid = "3234",
          satiksmeBotHeartbeat = (nowEpoch - 600).toString(),
          siteNotifierPid = "2234",
          siteNotifierHeartbeat = nowEpoch.toString(),
          vpnHealth = "1"
        ),
        stderr = ""
      )
    }

    val checker = RuntimeHealthChecker(runner)
    val snapshot = runBlocking { checker.check(StackConfigV1()) }

    assertFalse(snapshot.satiksmeBotHealthy)
    assertEquals("heartbeat_stale", snapshot.evidence["satiksme_bot_failure_reason"])
  }

  @Test
  fun marksRemoteHealthyInTokenizedModeWhenTokenPathIs200AndBarePathIsNon200() {
    val runner = CommandRunner {
      CommandResult(
        ok = true,
        stdout = probeOutput(
          idU = "0",
          listeners = "LISTEN 0 128 0.0.0.0:53\nLISTEN 0 128 0.0.0.0:2222\nLISTEN 0 128 0.0.0.0:443\n",
          ddnsEpoch = (System.currentTimeMillis() / 1000).toString(),
          trainBotPid = "1234",
          trainBotHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          siteNotifierPid = "2234",
          siteNotifierHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          vpnHealth = "1",
          remoteDohTokenizedCode = "200",
          remoteDohBareCode = "404",
          remoteIdentityInjectCode = "200",
          remotePublicRootCode = "200",
          remotePublicProbeAvailable = "1",
          remotePublicDohTokenizedCode = "200",
          remotePublicDohBareCode = "404",
          remotePublicIdentityInjectCode = "200"
        ),
        stderr = ""
      )
    }

    val checker = RuntimeHealthChecker(runner)
    val config = StackConfigV1().copy(
      remote = StackConfigV1().remote.copy(
        dohEnabled = true,
        dohEndpointMode = "tokenized",
        dohPathToken = "0123456789abcdef0123456789abcdef",
        watchdogEscalateRuntimeRestart = true
      )
    )
    val snapshot = runBlocking { checker.check(config) }

    assertTrue(snapshot.remoteHealthy)
    assertTrue(snapshot.supervisorHealthy)
    assertEquals("tokenized", snapshot.evidence["doh_endpoint_mode"])
    assertEquals("no_query_http_contract", snapshot.evidence["doh_probe_mode"])
    assertEquals("true", snapshot.evidence["doh_contract"])
    assertEquals("true", snapshot.evidence["identity_frontend_required"])
    assertEquals("true", snapshot.evidence["identity_frontend_healthy"])
    assertEquals("true", snapshot.evidence["remote_public_doh_contract"])
    assertEquals("true", snapshot.evidence["remote_public_identity_frontend_healthy"])
  }

  @Test
  fun keepsRemoteHealthyWhenPublicRootRedirectsToLogin() {
    val runner = CommandRunner {
      CommandResult(
        ok = true,
        stdout = probeOutput(
          idU = "0",
          listeners = "LISTEN 0 128 0.0.0.0:53\nLISTEN 0 128 0.0.0.0:2222\nLISTEN 0 128 0.0.0.0:443\n",
          ddnsEpoch = (System.currentTimeMillis() / 1000).toString(),
          trainBotPid = "1234",
          trainBotHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          siteNotifierPid = "2234",
          siteNotifierHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          vpnHealth = "1",
          remoteDohTokenizedCode = "400",
          remoteDohBareCode = "404",
          remoteIdentityInjectCode = "200",
          remotePublicRootCode = "302",
          remotePublicProbeAvailable = "1",
          remotePublicDohTokenizedCode = "400",
          remotePublicDohBareCode = "404",
          remotePublicIdentityInjectCode = "200"
        ),
        stderr = ""
      )
    }

    val checker = RuntimeHealthChecker(runner)
    val config = StackConfigV1().copy(
      remote = StackConfigV1().remote.copy(
        dohEnabled = true,
        dohEndpointMode = "tokenized",
        dohPathToken = "0123456789abcdef0123456789abcdef",
        watchdogEscalateRuntimeRestart = true
      )
    )
    val snapshot = runBlocking { checker.check(config) }

    assertTrue(snapshot.remoteHealthy)
    assertTrue(snapshot.supervisorHealthy)
    assertEquals("302", snapshot.evidence["remote_public_root_code"])
    assertEquals("true", snapshot.evidence["remote_public_root_healthy"])
  }

  @Test
  fun keepsRemoteHealthyInTokenizedModeWhenPublicIdentityInjectEndpointIsNotHealthyButLocalIngressIsHealthy() {
    val runner = CommandRunner {
      CommandResult(
        ok = true,
        stdout = probeOutput(
          idU = "0",
          listeners = "LISTEN 0 128 0.0.0.0:53\nLISTEN 0 128 0.0.0.0:2222\nLISTEN 0 128 0.0.0.0:443\n",
          ddnsEpoch = (System.currentTimeMillis() / 1000).toString(),
          trainBotPid = "1234",
          trainBotHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          siteNotifierPid = "2234",
          siteNotifierHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          vpnHealth = "1",
          remoteDohTokenizedCode = "200",
          remoteDohBareCode = "404",
          remoteIdentityInjectCode = "200",
          remotePublicRootCode = "200",
          remotePublicProbeAvailable = "1",
          remotePublicDohTokenizedCode = "200",
          remotePublicDohBareCode = "404",
          remotePublicIdentityInjectCode = "502"
        ),
        stderr = ""
      )
    }

    val checker = RuntimeHealthChecker(runner)
    val config = StackConfigV1().copy(
      remote = StackConfigV1().remote.copy(
        dohEnabled = true,
        dohEndpointMode = "tokenized",
        dohPathToken = "0123456789abcdef0123456789abcdef",
        watchdogEscalateRuntimeRestart = true
      )
    )
    val snapshot = runBlocking { checker.check(config) }

    assertTrue(snapshot.remoteHealthy)
    assertTrue(snapshot.supervisorHealthy)
    assertEquals("true", snapshot.evidence["identity_frontend_required"])
    assertEquals("true", snapshot.evidence["identity_frontend_healthy"])
    assertEquals("false", snapshot.evidence["remote_public_identity_frontend_healthy"])
    assertEquals("502", snapshot.evidence["remote_public_identity_inject_code"])
  }

  @Test
  fun marksRemoteUnhealthyInTokenizedModeWhenBarePathReturns200() {
    val runner = CommandRunner {
      CommandResult(
        ok = true,
        stdout = probeOutput(
          idU = "0",
          listeners = "LISTEN 0 128 0.0.0.0:53\nLISTEN 0 128 0.0.0.0:2222\nLISTEN 0 128 0.0.0.0:443\n",
          ddnsEpoch = (System.currentTimeMillis() / 1000).toString(),
          trainBotPid = "1234",
          trainBotHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          siteNotifierPid = "2234",
          siteNotifierHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          vpnHealth = "1",
          remoteDohTokenizedCode = "200",
          remoteDohBareCode = "200",
          remotePublicRootCode = "200",
          remotePublicProbeAvailable = "1",
          remotePublicDohTokenizedCode = "200",
          remotePublicDohBareCode = "200",
          remotePublicIdentityInjectCode = "200"
        ),
        stderr = ""
      )
    }

    val checker = RuntimeHealthChecker(runner)
    val config = StackConfigV1().copy(
      remote = StackConfigV1().remote.copy(
        dohEnabled = true,
        dohEndpointMode = "tokenized",
        dohPathToken = "0123456789abcdef0123456789abcdef",
        watchdogEscalateRuntimeRestart = true
      )
    )
    val snapshot = runBlocking { checker.check(config) }

    assertFalse(snapshot.remoteHealthy)
    assertFalse(snapshot.supervisorHealthy)
    assertEquals("false", snapshot.evidence["doh_contract"])
    assertEquals("false", snapshot.evidence["remote_public_doh_contract"])
  }

  @Test
  fun marksRemoteUnhealthyInTokenizedModeWhenTokenPathIsRouteMiss() {
    val runner = CommandRunner {
      CommandResult(
        ok = true,
        stdout = probeOutput(
          idU = "0",
          listeners = "LISTEN 0 128 0.0.0.0:53\nLISTEN 0 128 0.0.0.0:2222\nLISTEN 0 128 0.0.0.0:443\n",
          ddnsEpoch = (System.currentTimeMillis() / 1000).toString(),
          trainBotPid = "1234",
          trainBotHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          siteNotifierPid = "2234",
          siteNotifierHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          vpnHealth = "1",
          remoteDohTokenizedCode = "404",
          remoteDohBareCode = "404"
        ),
        stderr = ""
      )
    }

    val checker = RuntimeHealthChecker(runner)
    val config = StackConfigV1().copy(
      remote = StackConfigV1().remote.copy(
        dohEnabled = true,
        dohEndpointMode = "tokenized",
        dohPathToken = "0123456789abcdef0123456789abcdef",
        watchdogEscalateRuntimeRestart = true
      )
    )
    val snapshot = runBlocking { checker.check(config) }

    assertFalse(snapshot.remoteHealthy)
    assertFalse(snapshot.supervisorHealthy)
    assertEquals("false", snapshot.evidence["doh_contract"])
  }

  @Test
  fun keepsSupervisorHealthyWhenRemoteIsNotRequiredAndRemotePortsMissing() {
    val runner = CommandRunner {
      CommandResult(
        ok = true,
        stdout = probeOutput(
          idU = "0",
          listeners = "LISTEN 0 128 0.0.0.0:53\nLISTEN 0 128 0.0.0.0:2222\n",
          ddnsEpoch = (System.currentTimeMillis() / 1000).toString(),
          trainBotPid = "1234",
          trainBotHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          siteNotifierPid = "2234",
          siteNotifierHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          vpnHealth = "1"
        ),
        stderr = ""
      )
    }

    val checker = RuntimeHealthChecker(runner)
    val config = StackConfigV1().copy(remote = StackConfigV1().remote.copy(dohEnabled = false, dotEnabled = false))
    val snapshot = runBlocking { checker.check(config) }

    assertTrue(snapshot.dnsHealthy)
    assertTrue(snapshot.sshHealthy)
    assertTrue(snapshot.vpnHealthy)
    assertTrue(snapshot.remoteHealthy)
    assertTrue(snapshot.supervisorHealthy)
  }

  @Test
  fun keepsRemoteHealthyWhenPublicRootIsUnavailableButLocalIngressIsHealthy() {
    val runner = CommandRunner {
      CommandResult(
        ok = true,
        stdout = probeOutput(
          idU = "0",
          listeners = "LISTEN 0 128 0.0.0.0:53\nLISTEN 0 128 0.0.0.0:2222\nLISTEN 0 128 0.0.0.0:443\n",
          ddnsEpoch = (System.currentTimeMillis() / 1000).toString(),
          trainBotPid = "1234",
          trainBotHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          siteNotifierPid = "2234",
          siteNotifierHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          vpnHealth = "1",
          remoteDohTokenizedCode = "200",
          remoteDohBareCode = "404",
          remoteIdentityInjectCode = "200",
          remotePublicRootCode = "000",
          remotePublicProbeAvailable = "1",
          remotePublicDohTokenizedCode = "200",
          remotePublicDohBareCode = "404",
          remotePublicIdentityInjectCode = "200"
        ),
        stderr = ""
      )
    }

    val checker = RuntimeHealthChecker(runner)
    val config = StackConfigV1().copy(
      remote = StackConfigV1().remote.copy(
        dohEnabled = true,
        dohEndpointMode = "tokenized",
        dohPathToken = "0123456789abcdef0123456789abcdef",
        watchdogEscalateRuntimeRestart = true
      )
    )
    val snapshot = runBlocking { checker.check(config) }

    assertTrue(snapshot.remoteHealthy)
    assertTrue(snapshot.supervisorHealthy)
    assertEquals("000", snapshot.evidence["remote_public_root_code"])
    assertEquals("false", snapshot.evidence["remote_public_root_healthy"])
  }

  @Test
  fun keepsRemoteHealthyWhenPublicTokenizedDohReturnsGatewayErrorButLocalIngressIsHealthy() {
    val runner = CommandRunner {
      CommandResult(
        ok = true,
        stdout = probeOutput(
          idU = "0",
          listeners = "LISTEN 0 128 0.0.0.0:53\nLISTEN 0 128 0.0.0.0:2222\nLISTEN 0 128 0.0.0.0:443\n",
          ddnsEpoch = (System.currentTimeMillis() / 1000).toString(),
          trainBotPid = "1234",
          trainBotHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          siteNotifierPid = "2234",
          siteNotifierHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          vpnHealth = "1",
          remoteDohTokenizedCode = "200",
          remoteDohBareCode = "404",
          remoteIdentityInjectCode = "200",
          remotePublicRootCode = "200",
          remotePublicProbeAvailable = "1",
          remotePublicDohTokenizedCode = "502",
          remotePublicDohBareCode = "404",
          remotePublicIdentityInjectCode = "200"
        ),
        stderr = ""
      )
    }

    val checker = RuntimeHealthChecker(runner)
    val config = StackConfigV1().copy(
      remote = StackConfigV1().remote.copy(
        dohEnabled = true,
        dohEndpointMode = "tokenized",
        dohPathToken = "0123456789abcdef0123456789abcdef",
        watchdogEscalateRuntimeRestart = true
      )
    )
    val snapshot = runBlocking { checker.check(config) }

    assertTrue(snapshot.remoteHealthy)
    assertTrue(snapshot.supervisorHealthy)
    assertEquals("502", snapshot.evidence["remote_public_doh_tokenized_code"])
    assertEquals("false", snapshot.evidence["remote_public_doh_contract"])
  }

  @Test
  fun marksTrainBotUnhealthyWhenFreshScheduleIsRequiredButMissing() {
    val runner = CommandRunner {
      CommandResult(
        ok = true,
        stdout = probeOutput(
          idU = "0",
          listeners = "LISTEN 0 128 0.0.0.0:53\nLISTEN 0 128 0.0.0.0:2222\n",
          ddnsEpoch = (System.currentTimeMillis() / 1000).toString(),
          trainBotPid = "1234",
          trainBotHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          trainBotScheduleRequired = "1",
          trainBotScheduleFresh = "0",
          trainBotScheduleServiceDate = "2026-02-28",
          trainBotScheduleRows = "0",
          siteNotifierPid = "2234",
          siteNotifierHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          vpnHealth = "1"
        ),
        stderr = ""
      )
    }

    val checker = RuntimeHealthChecker(runner)
    val snapshot = runBlocking { checker.check(StackConfigV1()) }

    assertFalse(snapshot.trainBotHealthy)
    assertEquals("true", snapshot.evidence["train_bot_schedule_required"])
    assertEquals("false", snapshot.evidence["train_bot_schedule_fresh"])
  }

  @Test
  fun keepsTrainBotHealthyWhenFreshScheduleIsNotRequiredYet() {
    val runner = CommandRunner {
      CommandResult(
        ok = true,
        stdout = probeOutput(
          idU = "0",
          listeners = "LISTEN 0 128 0.0.0.0:53\nLISTEN 0 128 0.0.0.0:2222\n",
          ddnsEpoch = (System.currentTimeMillis() / 1000).toString(),
          trainBotPid = "1234",
          trainBotHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          trainBotScheduleRequired = "0",
          trainBotScheduleFresh = "0",
          trainBotScheduleServiceDate = "2026-02-28",
          trainBotScheduleRows = "0",
          siteNotifierPid = "2234",
          siteNotifierHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          vpnHealth = "1"
        ),
        stderr = ""
      )
    }

    val checker = RuntimeHealthChecker(runner)
    val snapshot = runBlocking { checker.check(StackConfigV1()) }

    assertTrue(snapshot.trainBotHealthy)
  }

  @Test
  fun keepsTrainBotHealthyWhenFreshScheduleIsPresent() {
    val runner = CommandRunner {
      CommandResult(
        ok = true,
        stdout = probeOutput(
          idU = "0",
          listeners = "LISTEN 0 128 0.0.0.0:53\nLISTEN 0 128 0.0.0.0:2222\n",
          ddnsEpoch = (System.currentTimeMillis() / 1000).toString(),
          trainBotPid = "1234",
          trainBotHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          trainBotScheduleRequired = "1",
          trainBotScheduleFresh = "1",
          trainBotScheduleServiceDate = "2026-02-28",
          trainBotScheduleRows = "7",
          siteNotifierPid = "2234",
          siteNotifierHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          vpnHealth = "1"
        ),
        stderr = ""
      )
    }

    val checker = RuntimeHealthChecker(runner)
    val snapshot = runBlocking { checker.check(StackConfigV1()) }

    assertTrue(snapshot.trainBotHealthy)
    assertEquals("7", snapshot.evidence["train_bot_schedule_rows"])
  }

  @Test
  fun keepsTrainBotHealthyWhenScheduleRowsExistEvenIfFreshFileIsMissing() {
    val runner = CommandRunner {
      CommandResult(
        ok = true,
        stdout = probeOutput(
          idU = "0",
          listeners = "LISTEN 0 128 0.0.0.0:53\nLISTEN 0 128 0.0.0.0:2222\n",
          ddnsEpoch = (System.currentTimeMillis() / 1000).toString(),
          trainBotPid = "1234",
          trainBotHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          trainBotScheduleRequired = "1",
          trainBotScheduleFresh = "0",
          trainBotScheduleServiceDate = "2026-02-28",
          trainBotScheduleRows = "3",
          siteNotifierPid = "2234",
          siteNotifierHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          vpnHealth = "1"
        ),
        stderr = ""
      )
    }

    val checker = RuntimeHealthChecker(runner)
    val snapshot = runBlocking { checker.check(StackConfigV1()) }

    assertTrue(snapshot.trainBotHealthy)
    assertEquals("true", snapshot.evidence["train_bot_schedule_rows_present"])
  }

  @Test
  fun keepsTrainBotHealthyWhenScheduleRowsAreUnknownButFreshFileExists() {
    val runner = CommandRunner {
      CommandResult(
        ok = true,
        stdout = probeOutput(
          idU = "0",
          listeners = "LISTEN 0 128 0.0.0.0:53\nLISTEN 0 128 0.0.0.0:2222\n",
          ddnsEpoch = (System.currentTimeMillis() / 1000).toString(),
          trainBotPid = "1234",
          trainBotHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          trainBotScheduleRequired = "1",
          trainBotScheduleFresh = "1",
          trainBotScheduleServiceDate = "2026-02-28",
          trainBotScheduleRows = "unknown",
          siteNotifierPid = "2234",
          siteNotifierHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          vpnHealth = "1"
        ),
        stderr = ""
      )
    }

    val checker = RuntimeHealthChecker(runner)
    val snapshot = runBlocking { checker.check(StackConfigV1()) }

    assertTrue(snapshot.trainBotHealthy)
    assertEquals("unknown", snapshot.evidence["train_bot_schedule_rows"])
  }

  @Test
  fun keepsTrainBotHealthyWhenScheduleProbeIsInconclusive() {
    val runner = CommandRunner {
      CommandResult(
        ok = true,
        stdout = probeOutput(
          idU = "0",
          listeners = "LISTEN 0 128 0.0.0.0:53\nLISTEN 0 128 0.0.0.0:2222\n",
          ddnsEpoch = (System.currentTimeMillis() / 1000).toString(),
          trainBotPid = "1234",
          trainBotHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          trainBotScheduleRequired = "1",
          trainBotScheduleFresh = "0",
          trainBotScheduleServiceDate = "2026-02-28",
          trainBotScheduleRows = "unknown",
          siteNotifierPid = "2234",
          siteNotifierHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          vpnHealth = "1"
        ),
        stderr = ""
      )
    }

    val checker = RuntimeHealthChecker(runner)
    val snapshot = runBlocking { checker.check(StackConfigV1()) }

    assertTrue(snapshot.trainBotHealthy)
    assertEquals("true", snapshot.evidence["train_bot_schedule_probe_inconclusive"])
  }

  @Test
  fun keepsTrainBotHealthyWhenTunnelModeHasLivePidAndPublicPagesReturn200() {
    val runner = CommandRunner {
      CommandResult(
        ok = true,
        stdout = probeOutput(
          idU = "0",
          listeners = "LISTEN 0 128 0.0.0.0:53\nLISTEN 0 128 0.0.0.0:2222\n",
          ddnsEpoch = (System.currentTimeMillis() / 1000).toString(),
          trainBotPid = "1234",
          trainBotTunnelEnabled = "1",
          trainBotTunnelSupervisorPid = "4444",
          trainBotTunnelPid = "5555",
          trainBotTunnelPublicBaseUrl = "https://train-bot.example.com",
          trainBotPublicRootCode = "200",
          trainBotPublicAppCode = "200",
          trainBotTunnelProbeAvailable = "1",
          trainBotHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          siteNotifierPid = "2234",
          siteNotifierHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          vpnHealth = "1"
        ),
        stderr = ""
      )
    }

    val checker = RuntimeHealthChecker(runner)
    val snapshot = runBlocking { checker.check(StackConfigV1()) }

    assertTrue(snapshot.trainBotHealthy)
    assertEquals("true", snapshot.evidence["train_bot_tunnel_healthy"])
    assertEquals("200", snapshot.evidence["train_bot_public_root_code"])
    assertEquals("200", snapshot.evidence["train_bot_public_app_code"])
  }

  @Test
  fun marksTrainBotUnhealthyWhenTunnelModeHasNoLiveTunnelPid() {
    val runner = CommandRunner {
      CommandResult(
        ok = true,
        stdout = probeOutput(
          idU = "0",
          listeners = "LISTEN 0 128 0.0.0.0:53\nLISTEN 0 128 0.0.0.0:2222\n",
          ddnsEpoch = (System.currentTimeMillis() / 1000).toString(),
          trainBotPid = "1234",
          trainBotTunnelEnabled = "1",
          trainBotTunnelSupervisorPid = "4444",
          trainBotTunnelPid = "",
          trainBotTunnelPublicBaseUrl = "https://train-bot.example.com",
          trainBotPublicRootCode = "200",
          trainBotPublicAppCode = "200",
          trainBotTunnelProbeAvailable = "1",
          trainBotHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          siteNotifierPid = "2234",
          siteNotifierHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          vpnHealth = "1"
        ),
        stderr = ""
      )
    }

    val checker = RuntimeHealthChecker(runner)
    val snapshot = runBlocking { checker.check(StackConfigV1()) }

    assertFalse(snapshot.trainBotHealthy)
    assertEquals("false", snapshot.evidence["train_bot_tunnel_healthy"])
  }

  @Test
  fun marksTrainBotUnhealthyWhenTunnelModePublicProbeFails() {
    val runner = CommandRunner {
      CommandResult(
        ok = true,
        stdout = probeOutput(
          idU = "0",
          listeners = "LISTEN 0 128 0.0.0.0:53\nLISTEN 0 128 0.0.0.0:2222\n",
          ddnsEpoch = (System.currentTimeMillis() / 1000).toString(),
          trainBotPid = "1234",
          trainBotTunnelEnabled = "1",
          trainBotTunnelSupervisorPid = "4444",
          trainBotTunnelPid = "5555",
          trainBotTunnelPublicBaseUrl = "https://train-bot.example.com",
          trainBotPublicRootCode = "530",
          trainBotPublicAppCode = "530",
          trainBotTunnelProbeAvailable = "1",
          trainBotHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          siteNotifierPid = "2234",
          siteNotifierHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          vpnHealth = "1"
        ),
        stderr = ""
      )
    }

    val checker = RuntimeHealthChecker(runner)
    val snapshot = runBlocking { checker.check(StackConfigV1()) }

    assertFalse(snapshot.trainBotHealthy)
    assertEquals("false", snapshot.evidence["train_bot_tunnel_healthy"])
    assertEquals("530", snapshot.evidence["train_bot_public_root_code"])
  }

  @Test
  fun marksTrainBotUnhealthyWhenTunnelSupervisorPidIsMissing() {
    val runner = CommandRunner {
      CommandResult(
        ok = true,
        stdout = probeOutput(
          idU = "0",
          listeners = "LISTEN 0 128 0.0.0.0:53\nLISTEN 0 128 0.0.0.0:2222\n",
          ddnsEpoch = (System.currentTimeMillis() / 1000).toString(),
          trainBotPid = "1234",
          trainBotTunnelEnabled = "1",
          trainBotTunnelSupervisorPid = "",
          trainBotTunnelPid = "5555",
          trainBotTunnelPublicBaseUrl = "https://train-bot.example.com",
          trainBotPublicRootCode = "200",
          trainBotPublicAppCode = "200",
          trainBotTunnelProbeAvailable = "1",
          trainBotHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          siteNotifierPid = "2234",
          siteNotifierHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          vpnHealth = "1"
        ),
        stderr = ""
      )
    }

    val checker = RuntimeHealthChecker(runner)
    val snapshot = runBlocking { checker.check(StackConfigV1()) }

    assertFalse(snapshot.trainBotHealthy)
    assertEquals("false", snapshot.evidence["train_bot_tunnel_supervisor_healthy"])
  }

  @Test
  fun doesNotRequireTunnelProbeWhenTunnelModeIsDisabled() {
    val runner = CommandRunner {
      CommandResult(
        ok = true,
        stdout = probeOutput(
          idU = "0",
          listeners = "LISTEN 0 128 0.0.0.0:53\nLISTEN 0 128 0.0.0.0:2222\n",
          ddnsEpoch = (System.currentTimeMillis() / 1000).toString(),
          trainBotPid = "1234",
          trainBotTunnelEnabled = "0",
          trainBotHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          siteNotifierPid = "2234",
          siteNotifierHeartbeat = (System.currentTimeMillis() / 1000).toString(),
          vpnHealth = "1"
        ),
        stderr = ""
      )
    }

    val checker = RuntimeHealthChecker(runner)
    val snapshot = runBlocking { checker.check(StackConfigV1()) }

    assertTrue(snapshot.trainBotHealthy)
    assertEquals("true", snapshot.evidence["train_bot_tunnel_healthy"])
  }

  private fun probeOutput(
    idU: String,
    listeners: String,
    ddnsEpoch: String,
    trainBotPid: String,
    trainBotTunnelEnabled: String = "0",
    trainBotTunnelSupervisorPid: String = "",
    trainBotTunnelPid: String = "",
    trainBotTunnelPublicBaseUrl: String = "",
    trainBotPublicRootCode: String = "000",
    trainBotPublicAppCode: String = "000",
    trainBotTunnelProbeAvailable: String = "0",
    trainBotHeartbeat: String,
    trainBotScheduleRequired: String = "0",
    trainBotScheduleFresh: String = "1",
    trainBotScheduleServiceDate: String = "2026-02-28",
    trainBotScheduleRows: String = "1",
    satiksmeBotPid: String = "3234",
    satiksmeBotTunnelEnabled: String = "0",
    satiksmeBotTunnelSupervisorPid: String = "",
    satiksmeBotTunnelPid: String = "",
    satiksmeBotTunnelPublicBaseUrl: String = "",
    satiksmeBotPublicRootCode: String = "000",
    satiksmeBotPublicAppCode: String = "000",
    satiksmeBotTunnelProbeAvailable: String = "0",
    satiksmeBotHeartbeat: String = trainBotHeartbeat,
    siteNotifierPid: String,
    siteNotifierHeartbeat: String,
    vpnHealth: String,
    vpnEnabledEffective: String = "1",
    vpnTailscaledLive: String = "1",
    vpnTailscaledSock: String = "1",
    vpnTailnetIpv4: String = "100.64.0.10",
    vpnGuardChainIpv4: String = "1",
    vpnGuardChainIpv6: String = "1",
    remoteDohTokenizedCode: String = "404",
    remoteDohBareCode: String = "200",
    remoteIdentityInjectCode: String = "000",
    remotePublicBaseUrl: String = "https://dns.example.com",
    remotePublicRootCode: String = "200",
    remotePublicProbeAvailable: String = "1",
    remotePublicDohTokenizedCode: String = "404",
    remotePublicDohBareCode: String = "200",
    remotePublicIdentityInjectCode: String = "000"
  ): String {
    return buildString {
      appendLine("__PIXEL_HEALTH_ID_U__")
      appendLine(idU)
      appendLine("__PIXEL_HEALTH_LISTENERS__")
      append(listeners)
      appendLine("__PIXEL_HEALTH_DDNS_EPOCH__")
      appendLine(ddnsEpoch)
      appendLine("__PIXEL_HEALTH_TRAIN_BOT_PID__")
      appendLine(trainBotPid)
      appendLine("__PIXEL_HEALTH_TRAIN_BOT_TUNNEL_ENABLED__")
      appendLine(trainBotTunnelEnabled)
      appendLine("__PIXEL_HEALTH_TRAIN_BOT_TUNNEL_SUPERVISOR_PID__")
      appendLine(trainBotTunnelSupervisorPid)
      appendLine("__PIXEL_HEALTH_TRAIN_BOT_TUNNEL_PID__")
      appendLine(trainBotTunnelPid)
      appendLine("__PIXEL_HEALTH_TRAIN_BOT_TUNNEL_PUBLIC_BASE_URL__")
      appendLine(trainBotTunnelPublicBaseUrl)
      appendLine("__PIXEL_HEALTH_TRAIN_BOT_PUBLIC_ROOT_CODE__")
      appendLine(trainBotPublicRootCode)
      appendLine("__PIXEL_HEALTH_TRAIN_BOT_PUBLIC_APP_CODE__")
      appendLine(trainBotPublicAppCode)
      appendLine("__PIXEL_HEALTH_TRAIN_BOT_TUNNEL_PROBE_AVAILABLE__")
      appendLine(trainBotTunnelProbeAvailable)
      appendLine("__PIXEL_HEALTH_TRAIN_BOT_HEARTBEAT__")
      appendLine(trainBotHeartbeat)
      appendLine("__PIXEL_HEALTH_TRAIN_BOT_SCHEDULE_REQUIRED__")
      appendLine(trainBotScheduleRequired)
      appendLine("__PIXEL_HEALTH_TRAIN_BOT_SCHEDULE_FRESH__")
      appendLine(trainBotScheduleFresh)
      appendLine("__PIXEL_HEALTH_TRAIN_BOT_SCHEDULE_SERVICE_DATE__")
      appendLine(trainBotScheduleServiceDate)
      appendLine("__PIXEL_HEALTH_TRAIN_BOT_SCHEDULE_ROWS__")
      appendLine(trainBotScheduleRows)
      appendLine("__PIXEL_HEALTH_SATIKSME_BOT_PID__")
      appendLine(satiksmeBotPid)
      appendLine("__PIXEL_HEALTH_SATIKSME_BOT_TUNNEL_ENABLED__")
      appendLine(satiksmeBotTunnelEnabled)
      appendLine("__PIXEL_HEALTH_SATIKSME_BOT_TUNNEL_SUPERVISOR_PID__")
      appendLine(satiksmeBotTunnelSupervisorPid)
      appendLine("__PIXEL_HEALTH_SATIKSME_BOT_TUNNEL_PID__")
      appendLine(satiksmeBotTunnelPid)
      appendLine("__PIXEL_HEALTH_SATIKSME_BOT_TUNNEL_PUBLIC_BASE_URL__")
      appendLine(satiksmeBotTunnelPublicBaseUrl)
      appendLine("__PIXEL_HEALTH_SATIKSME_BOT_PUBLIC_ROOT_CODE__")
      appendLine(satiksmeBotPublicRootCode)
      appendLine("__PIXEL_HEALTH_SATIKSME_BOT_PUBLIC_APP_CODE__")
      appendLine(satiksmeBotPublicAppCode)
      appendLine("__PIXEL_HEALTH_SATIKSME_BOT_TUNNEL_PROBE_AVAILABLE__")
      appendLine(satiksmeBotTunnelProbeAvailable)
      appendLine("__PIXEL_HEALTH_SATIKSME_BOT_HEARTBEAT__")
      appendLine(satiksmeBotHeartbeat)
      appendLine("__PIXEL_HEALTH_SITE_NOTIFIER_PID__")
      appendLine(siteNotifierPid)
      appendLine("__PIXEL_HEALTH_SITE_NOTIFIER_HEARTBEAT__")
      appendLine(siteNotifierHeartbeat)
      appendLine("__PIXEL_HEALTH_VPN_HEALTH__")
      appendLine(vpnHealth)
      appendLine("__PIXEL_HEALTH_VPN_ENABLED_EFFECTIVE__")
      appendLine(vpnEnabledEffective)
      appendLine("__PIXEL_HEALTH_VPN_TAILSCALED_LIVE__")
      appendLine(vpnTailscaledLive)
      appendLine("__PIXEL_HEALTH_VPN_TAILSCALED_SOCK__")
      appendLine(vpnTailscaledSock)
      appendLine("__PIXEL_HEALTH_VPN_TAILNET_IPV4__")
      appendLine(vpnTailnetIpv4)
      appendLine("__PIXEL_HEALTH_VPN_GUARD_CHAIN_IPV4__")
      appendLine(vpnGuardChainIpv4)
      appendLine("__PIXEL_HEALTH_VPN_GUARD_CHAIN_IPV6__")
      appendLine(vpnGuardChainIpv6)
      appendLine("__PIXEL_HEALTH_REMOTE_DOH_TOKENIZED_CODE__")
      appendLine(remoteDohTokenizedCode)
      appendLine("__PIXEL_HEALTH_REMOTE_DOH_BARE_CODE__")
      appendLine(remoteDohBareCode)
      appendLine("__PIXEL_HEALTH_REMOTE_IDENTITY_INJECT_CODE__")
      appendLine(remoteIdentityInjectCode)
      appendLine("__PIXEL_HEALTH_REMOTE_PUBLIC_BASE_URL__")
      appendLine(remotePublicBaseUrl)
      appendLine("__PIXEL_HEALTH_REMOTE_PUBLIC_ROOT_CODE__")
      appendLine(remotePublicRootCode)
      appendLine("__PIXEL_HEALTH_REMOTE_PUBLIC_PROBE_AVAILABLE__")
      appendLine(remotePublicProbeAvailable)
      appendLine("__PIXEL_HEALTH_REMOTE_PUBLIC_DOH_TOKENIZED_CODE__")
      appendLine(remotePublicDohTokenizedCode)
      appendLine("__PIXEL_HEALTH_REMOTE_PUBLIC_DOH_BARE_CODE__")
      appendLine(remotePublicDohBareCode)
      appendLine("__PIXEL_HEALTH_REMOTE_PUBLIC_IDENTITY_INJECT_CODE__")
      appendLine(remotePublicIdentityInjectCode)
      appendLine("__PIXEL_HEALTH_DONE__")
    }
  }
}
