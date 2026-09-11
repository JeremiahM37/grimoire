declare module '/vendor/editor.js' {
  export function createLiveEditor(options: {
    parent: HTMLElement;
    doc: string;
    callbacks: { onChange: (text: string) => void };
  }): { setValue(value: string): void; destroy(): void };
}
