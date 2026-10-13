import { useEffect, useRef } from 'react';

/**
 * The soft 3D world behind every screen: morphing glass blobs and floating spheres that lean
 * towards the pointer (or the phone's tilt).
 *
 * CSS rather than WebGL on purpose. A Three.js scene would add ~150 kB and a GPU context to a
 * study app that has to open on a 2 GB Android phone; shaded gradients on a handful of
 * compositor layers look the same at this blur level and cost nothing to load. Motion stops
 * entirely under prefers-reduced-motion (see theme.css).
 *
 * Positions are percentages, so the same scene composes on a phone and a 27" screen.
 */

interface Shape {
    kind: 'blob' | 'sphere';
    /** left/top/size in vw-ish percentages of the viewport */
    x: string;
    y: string;
    w: string;
    h?: string;
    /** parallax travel in px: bigger is closer to the viewer */
    depth: number;
    delay: number;
}

const SHAPES: Shape[] = [
    {
        kind: 'blob',
        x: '-14%',
        y: '-12%',
        w: 'max(46vw, 360px)',
        h: 'max(36vw, 300px)',
        depth: 10,
        delay: 0,
    },
    {
        kind: 'blob',
        x: '58%',
        y: '62%',
        w: 'max(52vw, 380px)',
        h: 'max(40vw, 320px)',
        depth: 14,
        delay: -6,
    },
    {
        kind: 'blob',
        x: '-18%',
        y: '70%',
        w: 'max(34vw, 280px)',
        h: 'max(30vw, 240px)',
        depth: 8,
        delay: -11,
    },
    {
        kind: 'blob',
        x: '70%',
        y: '-10%',
        w: 'max(30vw, 240px)',
        h: 'max(24vw, 200px)',
        depth: 6,
        delay: -3,
    },
    { kind: 'sphere', x: '84%', y: '6%', w: 'clamp(70px, 9vw, 130px)', depth: 26, delay: 0 },
    { kind: 'sphere', x: '4%', y: '60%', w: 'clamp(30px, 3.4vw, 54px)', depth: 34, delay: -3 },
    { kind: 'sphere', x: '46%', y: '86%', w: 'clamp(22px, 2.4vw, 38px)', depth: 40, delay: -5 },
    { kind: 'sphere', x: '30%', y: '8%', w: 'clamp(18px, 1.8vw, 28px)', depth: 44, delay: -1.5 },
];

export function Scene() {
    const ref = useRef<HTMLDivElement>(null);

    useEffect(() => {
        const el = ref.current;
        if (!el || window.matchMedia('(prefers-reduced-motion: reduce)').matches) return;

        let frame = 0;
        const set = (x: number, y: number) => {
            cancelAnimationFrame(frame);
            frame = requestAnimationFrame(() => {
                el.style.setProperty('--mx', x.toFixed(3));
                el.style.setProperty('--my', y.toFixed(3));
            });
        };
        const onPointer = (e: PointerEvent) =>
            set((e.clientX / window.innerWidth) * 2 - 1, (e.clientY / window.innerHeight) * 2 - 1);
        // Phones have no pointer to follow, so the scene leans with the device instead.
        const onTilt = (e: DeviceOrientationEvent) =>
            set(
                Math.max(-1, Math.min(1, (e.gamma ?? 0) / 30)),
                Math.max(-1, Math.min(1, ((e.beta ?? 45) - 45) / 30))
            );

        window.addEventListener('pointermove', onPointer, { passive: true });
        window.addEventListener('deviceorientation', onTilt, { passive: true });
        return () => {
            cancelAnimationFrame(frame);
            window.removeEventListener('pointermove', onPointer);
            window.removeEventListener('deviceorientation', onTilt);
        };
    }, []);

    return (
        <div ref={ref} className="scene" aria-hidden="true">
            {SHAPES.map((s, i) => (
                <span
                    key={i}
                    className={s.kind === 'blob' ? 'scene__blob' : 'scene__sphere'}
                    style={
                        {
                            left: s.x,
                            top: s.y,
                            width: s.w,
                            height: s.h ?? s.w,
                            '--depth': s.depth,
                            animationDelay: `${s.delay}s`,
                        } as React.CSSProperties
                    }
                />
            ))}
        </div>
    );
}
