import { useEffect } from 'react';

/** What counts as pressable. One delegated listener covers every one of them, present or future. */
const PRESSABLE = 'button, .btn, .card--hover, .sidebar__link, .topnav__link, .option, .row-item';

/**
 * A ripple of light from the exact point pressed, on every pressable element in the app.
 *
 * One document-level pointerdown listener rather than a wrapper component around every button:
 * nothing in the pages has to opt in, and there is no per-button React state. The ripple is a span
 * animated with transform and opacity only, removed when its animation ends.
 */
export function Ripple() {
    useEffect(() => {
        if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) return;

        const onDown = (event: PointerEvent) => {
            const host = (event.target as Element | null)?.closest<HTMLElement>(PRESSABLE);
            if (!host || host.matches(':disabled')) return;

            const rect = host.getBoundingClientRect();
            const size = Math.hypot(rect.width, rect.height) * 2;
            const dot = document.createElement('span');
            dot.className = 'ripple';
            dot.style.width = dot.style.height = `${size}px`;
            dot.style.left = `${event.clientX - rect.left}px`;
            dot.style.top = `${event.clientY - rect.top}px`;

            host.classList.add('ripple-host');
            host.appendChild(dot);
            dot.addEventListener('animationend', () => dot.remove(), { once: true });
        };

        document.addEventListener('pointerdown', onDown, { passive: true });
        return () => document.removeEventListener('pointerdown', onDown);
    }, []);

    return null;
}
