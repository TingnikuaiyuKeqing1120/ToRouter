import { useEffect, useId, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { companionWorld } from './mascot.generated';
import './stage.css';

export type CompanionScene = 'idle' | 'work' | 'surf' | 'retry' | 'error' | 'done';

export interface CompanionStageProps {
  scene: CompanionScene;
  theme: 'dark' | 'light';
  /** Pauses artwork only; request/routing state belongs to the parent. */
  paused?: boolean;
  onGreet?: () => void;
  onActivate?: () => void;
  className?: string;
}

const greetings: Record<CompanionScene, string> = {
  idle: '嘘，刚才梦到小鱼了。',
  work: '收到，马上接着干！',
  surf: '站稳啦，出发！',
  retry: '收到，正在修，马上就好。',
  error: '还在排查，有消息就告诉你。',
  done: '配合愉快，下次还一起！',
};

type Point = [number, number];
type Whale = { path: SVGPathElement; points: Point[][]; pivot: Point; scene: string };
type Greeting = { scene: CompanionScene; serial: number };

function moveTails(whales: Whale[], scene: CompanionScene, playTime: number) {
  for (const whale of whales) {
    if (whale.scene !== scene) continue;
    const t = playTime / 1000, [px, py] = whale.pivot;
    const phase = (t % 6.8) / 6.8;
    const amplitude = scene === 'surf' ? .085 : scene === 'error' ? .006 : scene === 'retry' ? .015 : .045;
    const angle = scene === 'idle'
      ? (phase > .65 ? Math.sin((phase - .65) / .35 * Math.PI * 2) * .045 : 0)
      : Math.sin(t * Math.PI * 2 / (scene === 'surf' ? 1.45 : 2.8)) * amplitude;
    const smooth = (value: number) => { const n = Math.max(0, Math.min(1, value)); return n * n * (3 - 2 * n); };
    const loops = whale.points.map(points => points.map(([x, y]): Point => {
      const weight = smooth((x - (px - 24)) / 20) * smooth((py + 8 - y) / 24), a = angle * weight;
      return [px + (x - px) * Math.cos(a) - (y - py) * Math.sin(a), py + (x - px) * Math.sin(a) + (y - py) * Math.cos(a)];
    }));
    const n = (value: number) => Number(value.toFixed(2));
    whale.path.setAttribute('d', loops.map(points => points.map((p, i) => {
      const prev = points[(i + points.length - 1) % points.length], next = points[(i + 1) % points.length];
      const a = Math.min(.25, .7 / Math.hypot(p[0] - prev[0], p[1] - prev[1]));
      const b = Math.min(.25, .7 / Math.hypot(next[0] - p[0], next[1] - p[1]));
      return `${i ? 'L' : 'M'}${n(p[0] + (prev[0] - p[0]) * a)} ${n(p[1] + (prev[1] - p[1]) * a)}Q${n(p[0])} ${n(p[1])} ${n(p[0] + (next[0] - p[0]) * b)} ${n(p[1] + (next[1] - p[1]) * b)}`;
    }).join('') + 'Z').join(''));
  }
}

function restartAnimations(element: Element | null, finiteOnly: boolean) {
  element?.getAnimations?.({ subtree: true }).forEach(animation => {
    // CSS transitions preserve the approved scene fade. Only entrance actions
    // restart on scene entry; greetings explicitly restart their happy loop.
    if ('animationName' in animation && (!finiteOnly || animation.effect?.getTiming().iterations !== Infinity)) {
      animation.currentTime = 0;
    }
  });
}

export function CompanionStage({ scene, theme, paused = false, onGreet, onActivate, className = '' }: CompanionStageProps) {
  const id = useId();
  const world = useMemo(() => companionWorld.split('__COMPANION_INSTANCE__').join(`companion-${id.replace(/[^\w-]/g, '_')}`), [id]);
  const stage = useRef<HTMLDivElement>(null);
  const whales = useRef<Whale[]>([]);
  const playTime = useRef(0);
  const greetingElapsed = useRef(0);
  const serial = useRef(0);
  const [greeting, setGreeting] = useState<Greeting | null>(null);
  const [announcement, setAnnouncement] = useState('');
  const [reducedMotion, setReducedMotion] = useState(() => typeof matchMedia === 'function' && matchMedia('(prefers-reduced-motion: reduce)').matches);
  const [hidden, setHidden] = useState(() => document.hidden);
  const stopped = paused || reducedMotion || hidden;
  const activeGreeting = greeting?.scene === scene ? greeting : null;
  const reaction = scene === 'idle' || scene === 'work' || scene === 'surf' ? 'cheer' : 'notice';

  useEffect(() => {
    const motion = typeof matchMedia === 'function' ? matchMedia('(prefers-reduced-motion: reduce)') : null;
    const onMotion = () => setReducedMotion(motion?.matches ?? false);
    const onVisibility = () => setHidden(document.hidden);
    motion?.addEventListener('change', onMotion);
    document.addEventListener('visibilitychange', onVisibility);
    onMotion();
    onVisibility();
    return () => {
      motion?.removeEventListener('change', onMotion);
      document.removeEventListener('visibilitychange', onVisibility);
    };
  }, []);

  useLayoutEffect(() => {
    // These data attributes are generated from trusted local geometry, never
    // populated from gateway/API data.
    whales.current = [...(stage.current?.querySelectorAll<SVGPathElement>('.whale-outline') ?? [])].map(path => ({
      path,
      points: JSON.parse(path.dataset.points ?? '[]') as Point[][],
      pivot: (path.dataset.pivot ?? '0,0').split(',').map(Number) as Point,
      scene: path.closest('.scene')?.getAttribute('data-scene') ?? '',
    }));
  }, [world]);

  useLayoutEffect(() => {
    stage.current?.querySelectorAll('.scene').forEach(element => {
      element.classList.toggle('is-active', element.getAttribute('data-scene') === scene);
    });
    restartAnimations(stage.current?.querySelector('.scene.is-active') ?? null, true);
    setGreeting(null);
    setAnnouncement('');
    greetingElapsed.current = 0;
  }, [scene, world]);

  useLayoutEffect(() => {
    if (activeGreeting && reaction === 'cheer') {
      restartAnimations(stage.current?.querySelector('.scene.is-active .hello-clawd') ?? null, false);
    }
  }, [activeGreeting, reaction]);

  useEffect(() => {
    if (stopped) return;
    let raf: number;
    let previous: number | null = null;
    let shapeElapsed = 0;
    const tick = (time: number) => {
      // Resume starts a fresh clock; background elapsed time never catches up.
      const dt = previous === null ? 0 : Math.max(0, time - previous);
      previous = time;
      playTime.current += dt;
      shapeElapsed += dt;
      if (shapeElapsed >= 40) { shapeElapsed = 0; moveTails(whales.current, scene, playTime.current); }
      if (activeGreeting) {
        greetingElapsed.current += dt;
        if (greetingElapsed.current >= 2100) { setGreeting(null); setAnnouncement(''); }
      }
      raf = requestAnimationFrame(tick);
    };
    raf = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(raf);
  }, [scene, stopped, activeGreeting]);

  const greet = () => {
    onActivate?.();
    if (stopped) { setAnnouncement('动画已暂停，继续播放后可以打招呼'); return; }
    greetingElapsed.current = 0;
    setGreeting({ scene, serial: ++serial.current });
    setAnnouncement(greetings[scene]);
    onGreet?.();
  };

  return (
    <div ref={stage} className={`companion-stage ${className}`} data-theme={theme} data-scene={scene}
      data-paused={String(stopped)} data-greeting={String(!!activeGreeting)} data-reaction={reaction}>
      <button className="stage-button" type="button" aria-label="摸摸蓝鲸和 Clawd" title="点点它们，打个招呼" onClick={greet}>
        <span className="greeting-caption" aria-hidden="true">{greetings[scene]}</span>
        <span dangerouslySetInnerHTML={{ __html: world }} />
      </button>
      <span className="companion-stage-live" role="status" aria-live="polite">{announcement}</span>
    </div>
  );
}

export default CompanionStage;
