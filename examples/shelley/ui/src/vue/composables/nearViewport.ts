// Sticky "has this element ever been near the viewport?" tracking, built on a
// single shared IntersectionObserver.
//
// Used to defer expensive hydration (e.g. @pierre/diffs FileDiff instances)
// until the user can actually see the result. In a huge conversation the
// message list contains hundreds of diffs; hydrating them all up front puts
// ~200k elements of shadow DOM into the document, which makes every viewport
// resize re-lay-out for seconds. Deferring keeps the typical case (reading
// the recent tail of the conversation) small.
//
// The flag is sticky: once an element has been near the viewport it stays
// "near" forever, so hydrated content is never torn down by scrolling away.
import { onBeforeUnmount, ref, watch, type Ref } from "vue";

// Start hydrating one viewport-height before the element scrolls into view.
const ROOT_MARGIN = "100%";

type Callback = () => void;
let sharedObserver: IntersectionObserver | null = null;
const callbacks = new WeakMap<Element, Callback>();
const pendingElements = new Set<Element>();

function reveal(element: Element): void {
  const cb = callbacks.get(element);
  if (!cb) return;
  callbacks.delete(element);
  pendingElements.delete(element);
  sharedObserver?.unobserve(element);
  cb();
}

// Printing must include the real tool cards, not blank geometry placeholders.
// One shared listener keeps this O(1) in listener count even for huge histories.
if (typeof window !== "undefined") {
  window.addEventListener("beforeprint", () => {
    for (const element of [...pendingElements]) reveal(element);
  });
}

function observer(): IntersectionObserver {
  if (!sharedObserver) {
    sharedObserver = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          if (!entry.isIntersecting) continue;
          reveal(entry.target);
        }
      },
      { rootMargin: ROOT_MARGIN },
    );
  }
  return sharedObserver;
}

// Returns a ref that flips to true once `el` comes within one viewport of
// view (and stays true). If IntersectionObserver is unavailable (jsdom), the
// ref is true immediately.
export function useNearViewport(el: Ref<Element | null>): Ref<boolean> {
  const printing =
    typeof window !== "undefined" &&
    typeof window.matchMedia === "function" &&
    window.matchMedia("print").matches;
  const near = ref(typeof IntersectionObserver === "undefined" || printing);

  watch(
    el,
    (element, prev) => {
      if (prev) {
        callbacks.delete(prev);
        pendingElements.delete(prev);
        sharedObserver?.unobserve(prev);
      }
      if (element && !near.value) {
        callbacks.set(element, () => {
          near.value = true;
        });
        pendingElements.add(element);
        observer().observe(element);
      }
    },
    { immediate: true, flush: "post" },
  );

  onBeforeUnmount(() => {
    const element = el.value;
    if (element) {
      callbacks.delete(element);
      pendingElements.delete(element);
      sharedObserver?.unobserve(element);
    }
  });

  return near;
}
