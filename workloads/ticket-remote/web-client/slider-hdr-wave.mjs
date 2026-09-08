import { CLIENT_HDR_CANVAS_FORMAT } from './client-hdr-renderer.mjs';

// A static, premultiplied HDR gradient. CSS moves it; no video frames or
// animation loop belong to this decoration, and the stream owns the device.
export const SLIDER_WAVE_SHADER = `
struct Output { @builtin(position) position: vec4<f32>, @location(0) uv: vec2<f32> }
@group(0) @binding(0) var<uniform> params: vec4<f32>;
@vertex fn vertexMain(@builtin(vertex_index) i: u32) -> Output {
  var points = array<vec2<f32>, 3>(vec2<f32>(-1,-1), vec2<f32>(3,-1), vec2<f32>(-1,3));
  let p = points[i];
  return Output(vec4<f32>(p,0,1), vec2<f32>((p.x+1)*0.5, (1-p.y)*0.5));
}
@fragment fn fragmentMain(input: Output) -> @location(0) vec4<f32> {
  let direction = vec2<f32>(0.965925826, 0.258819045);
  let size = params.yz;
  let position = 0.5 + dot((input.uv-vec2<f32>(0.5))*size, direction) / dot(size, direction);
  let alpha = 0.19 * clamp(1.0-abs(position-0.5)/0.15, 0.0, 1.0);
  let white = select(params.x, 1.055*pow(params.x, 1.0/2.4)-0.055, params.w > 0.5);
  return vec4<f32>(vec3<f32>(white*alpha), alpha);
}`;

export class SliderHDRWave {
  constructor(overlay, canvas) {
    this.overlay = overlay;
    this.canvas = canvas;
    this.device = null;
    this.context = null;
    this.buffer = null;
    this.pipeline = null;
    this.pending = false;
    this.failed = false;
    this.generation = 0;
    this.key = '';
    this.target = null;
  }

  hide() { delete this.overlay.dataset.hdrWave; this.key = ''; }

  update(renderer, boost, bounds) {
    const device = renderer?.device || null;
    if (device !== this.device) {
      this.dispose();
      this.device = device;
    }
    this.target = bounds && device && !this.failed ? { renderer, boost, bounds } : null;
    if (!this.target) { this.hide(); return; }
    if (this.pending) return;
    if (!this.pipeline) { void this.initialize(); return; }
    void this.show();
  }

  async initialize() {
    const generation = this.generation, device = this.device;
    this.pending = true;
    try {
      const configuration = this.target.renderer.context.getConfiguration();
      this.context = this.canvas.getContext('webgpu');
      this.context.configure({ device, format: CLIENT_HDR_CANVAS_FORMAT,
        colorSpace: configuration.colorSpace, toneMapping: { mode: 'extended' },
        alphaMode: 'premultiplied' });
      const actual = this.context.getConfiguration();
      if (actual.format !== CLIENT_HDR_CANVAS_FORMAT || actual.toneMapping?.mode !== 'extended' ||
          actual.alphaMode !== 'premultiplied' || actual.colorSpace !== configuration.colorSpace) throw new Error('wave_configuration');
      const module = device.createShaderModule({ code: SLIDER_WAVE_SHADER });
      const pipeline = await device.createRenderPipelineAsync({ layout: 'auto',
        vertex: { module, entryPoint: 'vertexMain' },
        fragment: { module, entryPoint: 'fragmentMain', targets: [{ format: CLIENT_HDR_CANVAS_FORMAT }] },
        primitive: { topology: 'triangle-list' } });
      if (generation !== this.generation) return;
      this.pipeline = pipeline;
      this.buffer = device.createBuffer({ size: 16, usage: GPUBufferUsage.UNIFORM | GPUBufferUsage.COPY_DST });
      this.binding = device.createBindGroup({ layout: pipeline.getBindGroupLayout(0),
        entries: [{ binding: 0, resource: { buffer: this.buffer } }] });
      if (this.target) await this.show();
    } catch (_) {
      if (generation === this.generation) this.fail();
    } finally {
      if (generation === this.generation) this.pending = false;
    }
  }

  async show() {
    const generation = this.generation;
    this.pending = true;
    try {
      if (!this.overlay.dataset.hdrWave) {
        this.overlay.dataset.hdrWave = 'true';
        // Let the newly visible canvas join composition before its one static paint.
        await new Promise(requestAnimationFrame);
      }
      if (generation === this.generation && this.target) this.draw();
    } catch (_) {
      if (generation === this.generation) this.fail();
    } finally {
      if (generation === this.generation) this.pending = false;
    }
  }

  draw() {
    const { renderer, boost, bounds } = this.target;
    this.overlay.dataset.hdrWave = 'true';
    const scale = globalThis.devicePixelRatio || 1;
    const width = Math.max(1, Math.round(bounds.width * 2.5 * scale));
    const height = Math.max(1, Math.round(bounds.height * scale));
    const key = `${width}:${height}:${boost}:${renderer.encodeOutput}`;
    if (this.key !== key) {
      this.canvas.width = width;
      this.canvas.height = height;
      this.device.queue.writeBuffer(this.buffer, 0, new Float32Array([boost, width, height, renderer.encodeOutput ? 1 : 0]));
      const commands = this.device.createCommandEncoder();
      const pass = commands.beginRenderPass({ colorAttachments: [{
        view: this.context.getCurrentTexture().createView(), loadOp: 'clear', storeOp: 'store',
        clearValue: { r: 0, g: 0, b: 0, a: 0 }
      }] });
      pass.setPipeline(this.pipeline);
      pass.setBindGroup(0, this.binding);
      pass.draw(3);
      pass.end();
      this.device.queue.submit([commands.finish()]);
      this.key = key;
    }
  }

  fail() {
    const device = this.device;
    this.dispose();
    this.device = device;
    this.failed = true;
  }

  dispose() {
    this.generation++;
    this.hide();
    this.buffer?.destroy();
    try { this.context?.unconfigure(); } catch (_) {}
    this.device = this.context = this.buffer = this.pipeline = this.binding = this.target = null;
    this.key = '';
    this.pending = this.failed = false;
  }
}
