package lv.jolkins.pixelorchestrator.coreconfig

import kotlin.test.assertEquals
import kotlin.test.assertTrue
import kotlinx.serialization.json.Json
import org.junit.Test

class StackConfigV1SerializationTest {

  private val json = Json { encodeDefaults = true; ignoreUnknownKeys = true }

  @Test
  fun roundTripsVpnAndSshAuthMode() {
    val input = StackConfigV1(
      ssh = SshConfig(authMode = "password_only"),
      vpn = VpnConfig(
        enabled = true,
        runtimeRoot = "/data/local/pixel-stack/vpn",
        authKeyFile = "/data/local/pixel-stack/conf/vpn/tailscale-authkey",
        interfaceName = "tailscale0",
        hostname = "pixel-node",
        advertiseTags = "tag:pixel",
        acceptRoutes = false,
        acceptDns = false
      )
    )

    val encoded = json.encodeToString(StackConfigV1.serializer(), input)
    val decoded = json.decodeFromString(StackConfigV1.serializer(), encoded)

    assertEquals("password_only", decoded.ssh.authMode)
    assertTrue(decoded.vpn.enabled)
    assertEquals("tailscale0", decoded.vpn.interfaceName)
    assertEquals("tag:pixel", decoded.vpn.advertiseTags)
  }
}
