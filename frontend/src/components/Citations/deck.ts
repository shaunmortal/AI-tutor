import type { OpenedExcerpt } from '@/types/api';

/** True when the excerpt is a slide of a .pptx deck: the link downloads the deck, not a page. */
export function isDeck(opened: OpenedExcerpt): boolean {
    if (opened.slide === null || !opened.url) return false;
    try {
        return new URL(opened.url).pathname.toLowerCase().endsWith('.pptx');
    } catch {
        return false;
    }
}
