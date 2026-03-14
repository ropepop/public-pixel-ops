package lv.jolkins.pixelorchestrator.app

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent

class BootReceiver : BroadcastReceiver() {
  override fun onReceive(context: Context, intent: Intent?) {
    when (intent?.action) {
      Intent.ACTION_BOOT_COMPLETED -> {
        SupervisorService.start(
          context = context,
          action = SupervisorService.ACTION_BOOT_RECOVER,
          bootEventToken = "${intent.action}:${System.currentTimeMillis()}"
        )
      }
    }
  }
}
