declare module '/vendor/editor.js' {
  export function createLiveEditor(options: {
    parent: HTMLElement;
    doc: string;
    callbacks: { onChange: (text: string) => void };
  }): { setValue(value: string): void; destroy(): void };
}

declare module 'graphology-layout-forceatlas2/iterate' {
  const iterate: (settings: Record<string, unknown>, nodes: Float32Array, edges: Float32Array) => unknown;
  export default iterate;
}
