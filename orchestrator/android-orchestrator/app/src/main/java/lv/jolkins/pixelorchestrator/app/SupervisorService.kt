package lv.jolkins.pixelorchestrator.app

import android.app.Service
import android.content.Context
import android.content.Intent
import android.os.IBinder
import android.util.Log
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.launch
import lv.jolkins.pixelorchestrator.health.HealthScope

class SupervisorService : Service() {
  private val serviceScope = CoroutineScope(SupervisorJob() + Dispatchers.Default)

  override fun onCreate() {
    super.onCreate()
    NotificationHelper.ensureChannel(this)
    startForeground(4001, NotificationHelper.buildForegroundNotification(this))
  }

  override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
    val action = intent?.action
    val commandAction = OrchestratorShellCommand.normalizeAction(intent?.getStringExtra(OrchestratorShellCommand.EXTRA_ACTION))
    val component = intent?.getStringExtra(EXTRA_COMPONENT).orEmpty()
    val bootEventToken = intent?.getStringExtra(EXTRA_BOOT_EVENT_TOKEN).orEmpty()
    val pixelRunId = intent?.getStringExtra(EXTRA_PIXEL_RUN_ID).orEmpty()
    val facade = AppGraph.facade(this)
    val resultAction = commandAction.ifBlank { OrchestratorShellCommand.fromSupervisorAction(action).orEmpty() }

    serviceScope.launch {
      val result = when (action) {
        null -> FacadeOperationResult(true, "Supervisor service restart with no action; leaving runtime unchanged")

        ACTION_BOOT_START -> {
          FacadeOperationResult(true, "Ignoring legacy boot start request")
        }

        ACTION_BOOT_RECOVER -> {
          if (shouldHandleBootStart(bootEventToken)) {
            facade.runHealthCheck(HealthScope.FULL)
          } else {
            FacadeOperationResult(true, "Ignoring duplicate boot recovery request")
          }
        }

        ACTION_BOOTSTRAP -> facade.bootstrapStack()
        ACTION_START_ALL -> facade.startAll()
        ACTION_STOP_ALL -> facade.stopAll()
        ACTION_HEALTH -> facade.runHealthCheck(HealthScope.FULL)
        ACTION_START_COMPONENT -> facade.startComponent(component)
        ACTION_STOP_COMPONENT -> facade.stopComponent(component)
        ACTION_RESTART_COMPONENT -> facade.restartComponent(component)
        ACTION_REDEPLOY_COMPONENT -> facade.redeployComponent(component)
        ACTION_HEALTH_COMPONENT -> facade.healthComponent(component)
        ACTION_SYNC_DDNS -> facade.syncDdnsNow()
        ACTION_EXPORT_BUNDLE -> facade.exportSupportBundle(includeSecrets = false)
        else -> FacadeOperationResult(false, "Unknown action: $action")
      }

      Log.i(
        TAG,
        "action=$action command_action=$resultAction component=$component success=${result.success} message=${result.message}"
      )
      result.healthSnapshot?.let { health ->
        Log.i(
          TAG,
          "health root=${health.rootGranted} dns=${health.dnsHealthy} ssh=${health.sshHealthy} vpn=${health.vpnHealthy} train=${health.trainBotHealthy} satiksme=${health.satiksmeBotHealthy} notifier=${health.siteNotifierHealthy} remote=${health.remoteHealthy} ddns=${health.ddnsHealthy} supervisor=${health.supervisorHealthy}"
        )
      }
      if (pixelRunId.isNotBlank() && resultAction.isNotBlank()) {
        facade.writeActionResult(pixelRunId, resultAction, component, result)
      }
    }

    return START_NOT_STICKY
  }

  override fun onDestroy() {
    serviceScope.cancel()
    super.onDestroy()
  }

  override fun onBind(intent: Intent?): IBinder? = null

  companion object {
    private const val TAG = "SupervisorService"
    private const val PREFS_NAME = "supervisor_service"
    private const val PREF_LAST_BOOT_EVENT_TOKEN = "last_boot_event_token"
    const val ACTION_BOOT_START = "lv.jolkins.pixelorchestrator.action.BOOT_START"
    const val ACTION_BOOT_RECOVER = "lv.jolkins.pixelorchestrator.action.BOOT_RECOVER"
    const val ACTION_BOOTSTRAP = "lv.jolkins.pixelorchestrator.action.BOOTSTRAP"
    const val ACTION_START_ALL = "lv.jolkins.pixelorchestrator.action.START_ALL"
    const val ACTION_STOP_ALL = "lv.jolkins.pixelorchestrator.action.STOP_ALL"
    const val ACTION_HEALTH = "lv.jolkins.pixelorchestrator.action.HEALTH"
    const val ACTION_START_COMPONENT = "lv.jolkins.pixelorchestrator.action.START_COMPONENT"
    const val ACTION_STOP_COMPONENT = "lv.jolkins.pixelorchestrator.action.STOP_COMPONENT"
    const val ACTION_RESTART_COMPONENT = "lv.jolkins.pixelorchestrator.action.RESTART_COMPONENT"
    const val ACTION_REDEPLOY_COMPONENT = "lv.jolkins.pixelorchestrator.action.REDEPLOY_COMPONENT"
    const val ACTION_HEALTH_COMPONENT = "lv.jolkins.pixelorchestrator.action.HEALTH_COMPONENT"
    const val ACTION_SYNC_DDNS = "lv.jolkins.pixelorchestrator.action.SYNC_DDNS"
    const val ACTION_EXPORT_BUNDLE = "lv.jolkins.pixelorchestrator.action.EXPORT_BUNDLE"
    const val EXTRA_COMPONENT = OrchestratorShellCommand.EXTRA_COMPONENT
    const val EXTRA_BOOT_EVENT_TOKEN = "orchestrator_boot_event_token"
    const val EXTRA_PIXEL_RUN_ID = OrchestratorShellCommand.EXTRA_PIXEL_RUN_ID

    fun start(
      context: Context,
      action: String,
      component: String = "",
      bootEventToken: String = "",
      pixelRunId: String = "",
      commandAction: String = ""
    ) {
      val intent = Intent(context, SupervisorService::class.java).setAction(action)
      if (commandAction.isNotBlank()) {
        intent.putExtra(OrchestratorShellCommand.EXTRA_ACTION, commandAction)
      }
      if (component.isNotBlank()) {
        intent.putExtra(EXTRA_COMPONENT, component)
      }
      if (bootEventToken.isNotBlank()) {
        intent.putExtra(EXTRA_BOOT_EVENT_TOKEN, bootEventToken)
      }
      if (pixelRunId.isNotBlank()) {
        intent.putExtra(EXTRA_PIXEL_RUN_ID, pixelRunId)
      }
      context.startForegroundService(intent)
    }
  }

  private fun shouldHandleBootStart(token: String): Boolean {
    if (token.isBlank()) {
      Log.w(TAG, "boot start request missing token; skipping full start")
      return false
    }
    val prefs = getSharedPreferences(PREFS_NAME, MODE_PRIVATE)
    val previous = prefs.getString(PREF_LAST_BOOT_EVENT_TOKEN, "").orEmpty()
    if (previous == token) {
      Log.i(TAG, "boot start request already handled token=$token")
      return false
    }
    prefs.edit().putString(PREF_LAST_BOOT_EVENT_TOKEN, token).apply()
    return true
  }
}
