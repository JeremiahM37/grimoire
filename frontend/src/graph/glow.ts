/**
 * Node program: a lit disc with a soft halo, drawn in one triangle per note.
 *
 * The halo is what makes a cluster read as a glowing mass instead of confetti. It is a falloff in the fragment shader of
 * the triangle sigma already draws, so it costs no extra draw call, texture or post-processing pass: the only price is
 * fill, bounded by `spread`. On dark themes the halo is additive (overlapping notes brighten), on light themes it is an
 * ordinary translucent tint, because adding light to a white page washes it out.
 */
import { NodeProgram } from 'sigma/rendering';
import type { ProgramInfo } from 'sigma/rendering';
import type { NodeDisplayData, RenderParams } from 'sigma/types';
import { floatColor } from 'sigma/utils';

export interface GlowStyle {
  /** Halo radius as a multiple of the node radius (1 = no halo). */
  spread: number;
  /** Halo strength at the node's rim, 0..1. */
  glow: number;
  /** 1 = the halo adds light (dark themes), 0 = it blends as a tint (light themes). */
  additive: number;
  /** How far the centre of the disc is lifted toward white, 0..1. */
  light: number;
}

const VERTEX = /* glsl */ `
attribute vec4 a_id;
attribute vec4 a_color;
attribute vec2 a_position;
attribute float a_size;
attribute float a_angle;

uniform mat3 u_matrix;
uniform float u_sizeRatio;
uniform float u_correctionRatio;
uniform float u_spread;

varying vec4 v_color;
varying vec2 v_diffVector;
varying float v_radius;

const float bias = 255.0 / 254.0;

void main() {
  float size = a_size * u_correctionRatio / u_sizeRatio * 4.0;
  #ifdef PICKING_MODE
  float spread = 1.0;
  #else
  float spread = u_spread;
  #endif
  vec2 diffVector = size * spread * vec2(cos(a_angle), sin(a_angle));
  gl_Position = vec4((u_matrix * vec3(a_position + diffVector, 1)).xy, 0, 1);
  v_diffVector = diffVector;
  v_radius = size / 2.0;
  #ifdef PICKING_MODE
  v_color = a_id;
  #else
  v_color = a_color;
  #endif
  v_color.a *= bias;
}
`;

const FRAGMENT = /* glsl */ `
precision highp float;

varying vec4 v_color;
varying vec2 v_diffVector;
varying float v_radius;

uniform float u_correctionRatio;
uniform float u_spread;
uniform float u_glow;
uniform float u_additive;
uniform float u_light;

void main(void) {
  float border = u_correctionRatio * 2.0;
  float len = length(v_diffVector);
  float dist = len - v_radius + border;

  #ifdef PICKING_MODE
  if (dist > border) gl_FragColor = vec4(0.0);
  else gl_FragColor = v_color;
  #else
  float core = 1.0 - clamp(dist / border, 0.0, 1.0);
  float d = len / v_radius;
  float halo = clamp(1.0 - (d - 1.0) / max(u_spread - 1.0, 0.001), 0.0, 1.0);
  halo = halo * halo * halo * u_glow * (1.0 - core);
  vec3 lit = mix(v_color.rgb, vec3(1.0), (1.0 - smoothstep(0.0, 0.9, d)) * u_light);
  // premultiplied output: sigma blends with (ONE, ONE_MINUS_SRC_ALPHA), so zero alpha with colour adds light
  gl_FragColor = vec4((lit * core + v_color.rgb * halo) * v_color.a, (core + halo * (1.0 - u_additive)) * v_color.a);
  #endif
}
`;

const { UNSIGNED_BYTE, FLOAT } = WebGLRenderingContext;
const UNIFORMS = ['u_sizeRatio', 'u_correctionRatio', 'u_matrix', 'u_spread', 'u_glow', 'u_additive', 'u_light'] as const;

/** `style` is read on every frame, so changing it (theme switch) only needs a refresh. */
export function createNodeGlowProgram(style: GlowStyle) {
  return class NodeGlowProgram extends NodeProgram<(typeof UNIFORMS)[number]> {
    getDefinition() {
      return {
        VERTICES: 3,
        VERTEX_SHADER_SOURCE: VERTEX,
        FRAGMENT_SHADER_SOURCE: FRAGMENT,
        METHOD: WebGLRenderingContext.TRIANGLES,
        UNIFORMS,
        ATTRIBUTES: [
          { name: 'a_position', size: 2, type: FLOAT },
          { name: 'a_size', size: 1, type: FLOAT },
          { name: 'a_color', size: 4, type: UNSIGNED_BYTE, normalized: true },
          { name: 'a_id', size: 4, type: UNSIGNED_BYTE, normalized: true },
        ],
        CONSTANT_ATTRIBUTES: [{ name: 'a_angle', size: 1, type: FLOAT }],
        CONSTANT_DATA: [[0], [(2 * Math.PI) / 3], [(4 * Math.PI) / 3]],
      };
    }
    processVisibleItem(nodeIndex: number, startIndex: number, data: NodeDisplayData) {
      const array = this.array;
      array[startIndex++] = data.x;
      array[startIndex++] = data.y;
      array[startIndex++] = data.size;
      array[startIndex++] = floatColor(data.color);
      array[startIndex++] = nodeIndex;
    }
    setUniforms(params: RenderParams, { gl, uniformLocations }: ProgramInfo) {
      // the picking program does not use the halo uniforms, so their locations may be absent: null is a no-op
      const at = (name: (typeof UNIFORMS)[number]) => uniformLocations[name] ?? null;
      gl.uniform1f(at('u_correctionRatio'), params.correctionRatio);
      gl.uniform1f(at('u_sizeRatio'), params.sizeRatio);
      gl.uniformMatrix3fv(at('u_matrix'), false, params.matrix);
      gl.uniform1f(at('u_spread'), style.spread);
      gl.uniform1f(at('u_glow'), style.glow);
      gl.uniform1f(at('u_additive'), style.additive);
      gl.uniform1f(at('u_light'), style.light);
    }
  };
}
