// FakeEventSource stands in for the browser's EventSource, which jsdom lacks.
export class FakeEventSource {
  static instances: FakeEventSource[] = [];
  onopen: (() => void) | null = null;
  closed = false;
  private listeners = new Map<string, ((e: MessageEvent<string> | Event) => void)[]>();

  constructor(readonly url: string) {
    FakeEventSource.instances.push(this);
  }

  addEventListener(type: string, fn: (e: MessageEvent<string> | Event) => void) {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), fn]);
  }

  close() {
    this.closed = true;
  }

  open() {
    this.onopen?.();
  }

  emit(type: string, data: unknown) {
    const event = new MessageEvent(type, { data: typeof data === "string" ? data : JSON.stringify(data) });
    for (const fn of this.listeners.get(type) ?? []) fn(event);
  }

  // A connection failure: an error event that carries no data.
  drop() {
    for (const fn of this.listeners.get("error") ?? []) fn(new Event("error"));
  }

  static latest(): FakeEventSource {
    const s = FakeEventSource.instances.at(-1);
    if (!s) throw new Error("no EventSource opened");
    return s;
  }
}
