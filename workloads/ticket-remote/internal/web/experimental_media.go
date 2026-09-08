package web

import (
	"ticketremote/internal/auth"
	"ticketremote/internal/state"
)

const experimentalHDRTargetDisplayBoost = 4

func experimentalMediaCapability(id auth.Identity, snapshot state.Snapshot) map[string]any {
	selectedDisplayBoost := uint32(experimentalHDRTargetDisplayBoost)
	if _, ok := snapshot.Member(id.Email); ok {
		selectedDisplayBoost = snapshot.HDRDisplayBoostForAccountScope(ticketAccountScopeID(id.Email))
	}
	return map[string]any{
		"ok":                   true,
		"allowed":              true,
		"pipelineVersion":      "webgpu-mainthread-edr-v2",
		"visualMode":           "browser-hdr-canvas-v2",
		"mimeType":             "video/h264",
		"requiresHDR":          false,
		"targetDisplayBoost":   experimentalHDRTargetDisplayBoost,
		"allowedDisplayBoosts": []uint32{2, 3, 4, 5, 6},
		"selectedDisplayBoost": selectedDisplayBoost,
		"allowedEngines":       []string{"client_webgpu_v2"},
		"selectedEngine":       "client_webgpu_v2",
		"clientPipeline":       "webgpu-mainthread-edr-v2",
		"presentationKind":     "sdr_to_edr",
	}
}
