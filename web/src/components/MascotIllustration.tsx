import { memo } from 'react';
import { WHALE_BLUE_PATH, WHALE_WHITE_PATH } from './BrandLogo';

export type MascotVariant =
  | 'channel'
  | 'log'
  | 'search'
  | 'key'
  | 'model'
  | 'chart'
  | 'default';

export interface MascotIllustrationProps {
  variant?: MascotVariant;
  className?: string;
  size?: number;
}

// Reusable DeepSeek Pixel Whale
function WhaleSprite({
  x = 0,
  y = 0,
  scale = 0.9,
  sleeping = false,
  className = '',
}: {
  x?: number;
  y?: number;
  scale?: number;
  sleeping?: boolean;
  className?: string;
}) {
  return (
    <g transform={`translate(${x}, ${y}) scale(${scale})`} className={className}>
      <path d={WHALE_WHITE_PATH} fill="#FFFFFF" />
      <path d={WHALE_BLUE_PATH} fill="#4D6BFE" />
      {sleeping && (
        <g stroke="#1E293B" strokeWidth="1.5" strokeLinecap="round">
          {/* Sleeping curved eye */}
          <path d="M34,29 Q36,27 38,29" fill="none" />
        </g>
      )}
    </g>
  );
}

// Reusable Anthropic Clawd Mascot
function ClawdSprite({
  x = 0,
  y = 0,
  scale = 2,
  holding = 'none',
  sleeping = false,
  className = '',
}: {
  x?: number;
  y?: number;
  scale?: number;
  holding?: 'none' | 'wrench' | 'glass' | 'key';
  sleeping?: boolean;
  className?: string;
}) {
  const clawdBody = '#E07A5F';
  const headphoneBand = '#38BDF8';
  const headphoneCups = '#1D5C83';
  const clawdEye = '#0F172A';

  return (
    <g transform={`translate(${x}, ${y}) scale(${scale})`} className={className}>
      {/* Little legs */}
      <rect x="3" y="13" width="1" height="2" fill={clawdBody} />
      <rect x="5" y="13" width="1" height="2" fill={clawdBody} />
      <rect x="9" y="13" width="1" height="2" fill={clawdBody} />
      <rect x="11" y="13" width="1" height="2" fill={clawdBody} />
      {/* Body */}
      <rect x="2" y="6" width="11" height="7" fill={clawdBody} />
      {/* Left & right claws */}
      <rect x="0" y="9" width="2" height="2" fill={clawdBody} />
      <rect x="13" y="9" width="2" height="2" fill={clawdBody} />
      {/* Headphone */}
      <path d="M2.5 5 C 2.5 2, 12.5 2, 12.5 5" stroke={headphoneBand} strokeWidth="1.2" fill="none" />
      <rect x="0.8" y="5" width="2" height="3.5" rx="0.5" fill={headphoneCups} />
      <rect x="12.2" y="5" width="2" height="3.5" rx="0.5" fill={headphoneCups} />
      {/* Eyes or Sleepy Eyes */}
      {sleeping ? (
        <g stroke={clawdEye} strokeWidth="0.8" strokeLinecap="round">
          <path d="M4 9 Q 4.7 8.2 5.5 9" fill="none" />
          <path d="M9.5 9 Q 10.2 8.2 11 9" fill="none" />
        </g>
      ) : (
        <>
          <rect x="4" y="8" width="1.5" height="1.5" fill={clawdEye} />
          <rect x="9.5" y="8" width="1.5" height="1.5" fill={clawdEye} />
        </>
      )}

      {/* Held items */}
      {holding === 'wrench' && (
        <g transform="translate(14, 5)">
          <line x1="0" y1="5" x2="4" y2="1" stroke="#F59E0B" strokeWidth="1" strokeLinecap="round" />
          <circle cx="4" cy="1" r="1.5" fill="none" stroke="#F59E0B" strokeWidth="0.8" />
        </g>
      )}
      {holding === 'glass' && (
        <g transform="translate(13, 6)">
          <circle cx="3" cy="2" r="2.5" fill="none" stroke="#38BDF8" strokeWidth="0.8" />
          <line x1="5" y1="4" x2="7" y2="6" stroke="#38BDF8" strokeWidth="0.8" strokeLinecap="round" />
        </g>
      )}
      {holding === 'key' && (
        <g transform="translate(14, 4)">
          <circle cx="2" cy="1.5" r="1.5" fill="none" stroke="#F59E0B" strokeWidth="0.8" />
          <line x1="2" y1="3" x2="2" y2="7" stroke="#F59E0B" strokeWidth="0.8" />
          <line x1="2" y1="5" x2="3.5" y2="5" stroke="#F59E0B" strokeWidth="0.8" />
        </g>
      )}
    </g>
  );
}

export const MascotIllustration = memo(function MascotIllustration({
  variant = 'default',
  className = '',
  size = 150,
}: MascotIllustrationProps) {
  return (
    <svg
      width={size}
      height={Math.round((size * 110) / 150)}
      viewBox="0 0 160 120"
      preserveAspectRatio="xMidYMid meet"
      fill="none"
      xmlns="http://www.w3.org/2000/svg"
      className={`select-none pointer-events-none ${className}`}
      aria-hidden="true"
    >
      <defs>
        {/* Soft radial glow aura */}
        <radialGradient id={`mascot-glow-${variant}`} cx="50%" cy="50%" r="50%">
          <stop offset="0%" stopColor="#4D6BFE" stopOpacity="0.14" />
          <stop offset="60%" stopColor="#4D6BFE" stopOpacity="0.04" />
          <stop offset="100%" stopColor="#4D6BFE" stopOpacity="0" />
        </radialGradient>
        <radialGradient id="mascot-amber-glow" cx="50%" cy="50%" r="50%">
          <stop offset="0%" stopColor="#F59E0B" stopOpacity="0.18" />
          <stop offset="100%" stopColor="#F59E0B" stopOpacity="0" />
        </radialGradient>
      </defs>

      {/* Ambient background glow */}
      <circle cx="80" cy="60" r="54" fill={`url(#mascot-glow-${variant})`} />

      {/* SCENE 1: channel (连接首个渠道 / 管道未建立) */}
      {variant === 'channel' && (
        <g>
          {/* Dashed connector line */}
          <line x1="36" y1="84" x2="124" y2="84" stroke="currentColor" strokeOpacity="0.15" strokeWidth="1.5" strokeDasharray="3 3" />
          <circle cx="36" cy="84" r="4" fill="#38BDF8" fillOpacity="0.6" />
          <circle cx="124" cy="84" r="4" fill="#10B981" fillOpacity="0.6" />
          {/* Signal packets waiting */}
          <rect x="74" y="82" width="4" height="4" rx="1" fill="#F59E0B" opacity="0.8" />
          {/* Whale floating left-top */}
          <WhaleSprite x={16} y={18} scale={0.78} className="animate-mascot-float" />
          {/* Clawd engineer on right holding wrench */}
          <ClawdSprite x={108} y={62} scale={2.2} holding="wrench" />
        </g>
      )}

      {/* SCENE 2: log (暂无匹配请求 / 深海待机静候) */}
      {variant === 'log' && (
        <g>
          {/* Deep sea bubbles rising */}
          <g opacity="0.75">
            <circle cx="68" cy="38" r="4" stroke="#38BDF8" strokeWidth="1" fill="none" />
            <circle cx="76" cy="22" r="5.5" stroke="#38BDF8" strokeWidth="1" fill="#38BDF8" fillOpacity="0.1" />
            <circle cx="86" cy="10" r="3" stroke="#818CF8" strokeWidth="0.8" fill="none" />
          </g>
          {/* Whale sleeping horizontally */}
          <WhaleSprite x={38} y={44} scale={0.92} sleeping={true} className="animate-mascot-float" />
          {/* Clawd sleeping cozily near whale */}
          <ClawdSprite x={112} y={68} scale={1.8} sleeping={true} />
          {/* Z z floaters */}
          <text x="126" y="60" fill="#E07A5F" opacity="0.7" fontSize="9" fontFamily="monospace" fontWeight="bold">z</text>
          <text x="132" y="52" fill="#E07A5F" opacity="0.5" fontSize="11" fontFamily="monospace" fontWeight="bold">Z</text>
        </g>
      )}

      {/* SCENE 3: search (未找到匹配 / 搜索无结果) */}
      {variant === 'search' && (
        <g>
          {/* Radar sweep arcs */}
          <path d="M50,70 A30,30 0 0,1 110,70" stroke="#38BDF8" strokeWidth="1" strokeDasharray="3 3" opacity="0.3" fill="none" />
          <path d="M62,70 A18,18 0 0,1 98,70" stroke="#38BDF8" strokeWidth="1" opacity="0.4" fill="none" />
          {/* Whale peeking left */}
          <WhaleSprite x={14} y={26} scale={0.72} className="animate-mascot-float" />
          {/* Clawd with magnifying glass searching */}
          <ClawdSprite x={86} y={54} scale={2.3} holding="glass" />
          {/* Empty dashed target bracket */}
          <rect x="122" y="32" width="16" height="16" rx="3" stroke="#94A3B8" strokeWidth="1" strokeDasharray="2 2" fill="none" opacity="0.5" />
          <text x="128" y="44" fill="#94A3B8" opacity="0.6" fontSize="10" fontFamily="sans-serif">?</text>
        </g>
      )}

      {/* SCENE 4: key (尚未创建 API Key / 凭据库为空) */}
      {variant === 'key' && (
        <g>
          {/* Vault / key aura */}
          <circle cx="80" cy="50" r="26" fill="url(#mascot-amber-glow)" />
          {/* Floating golden master key in center */}
          <g transform="translate(73, 34)">
            <circle cx="7" cy="6" r="5" fill="none" stroke="#F59E0B" strokeWidth="1.8" />
            <circle cx="7" cy="6" r="2" fill="#F59E0B" />
            <path d="M7,11 L7,24 L11,24 M7,17 L10,17 M7,21 L10,21" stroke="#F59E0B" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" />
          </g>
          {/* Whale overhead guarding */}
          <WhaleSprite x={16} y={14} scale={0.7} className="animate-mascot-float" />
          {/* Clawd sentinel at bottom */}
          <ClawdSprite x={98} y={64} scale={2.1} />
        </g>
      )}

      {/* SCENE 5: model (下游模型出站拓扑) */}
      {variant === 'model' && (
        <g>
          {/* Routing wire between nodes */}
          <path d="M40,56 C70,20 90,95 125,56" fill="none" stroke="#818CF8" strokeWidth="1.5" strokeDasharray="3 3" opacity="0.5" />
          {/* Node 1: Inbound */}
          <circle cx="40" cy="56" r="6" fill="#4D6BFE" fillOpacity="0.2" stroke="#4D6BFE" strokeWidth="1.2" />
          <rect x="38" y="54" width="4" height="4" fill="#4D6BFE" />
          {/* Node 2: Outbound */}
          <circle cx="125" cy="56" r="6" fill="#10B981" fillOpacity="0.2" stroke="#10B981" strokeWidth="1.2" />
          <rect x="123" y="54" width="4" height="4" fill="#10B981" />
          {/* Mascots */}
          <WhaleSprite x={16} y={20} scale={0.65} className="animate-mascot-float" />
          <ClawdSprite x={85} y={60} scale={2.2} />
        </g>
      )}

      {/* SCENE 6: chart (用量与趋势等待) */}
      {variant === 'chart' && (
        <g>
          {/* Subtle dotted grid and line */}
          <line x1="28" y1="92" x2="132" y2="92" stroke="currentColor" strokeOpacity="0.12" strokeWidth="1" />
          <polyline points="32,86 58,74 84,80 110,54 130,58" fill="none" stroke="#4D6BFE" strokeWidth="1.5" strokeDasharray="2 3" opacity="0.6" />
          {/* Spark dots */}
          <circle cx="58" cy="74" r="2.5" fill="#38BDF8" />
          <circle cx="110" cy="54" r="2.5" fill="#10B981" />
          {/* Mascots looking at horizon */}
          <WhaleSprite x={18} y={24} scale={0.75} className="animate-mascot-float" />
          <ClawdSprite x={102} y={58} scale={2.1} />
        </g>
      )}

      {/* SCENE 7: default (通用吉祥物伴侣) */}
      {variant === 'default' && (
        <g>
          {/* Shifting diagonal router packets */}
          <g opacity="0.8">
            <rect x="100" y="34" width="4.5" height="4.5" rx="1" fill="#38BDF8" />
            <rect x="88" y="48" width="5" height="5" rx="1.2" fill="#10B981" />
            <rect x="74" y="62" width="5.5" height="5.5" rx="1.5" fill="#F59E0B" />
            <rect x="60" y="76" width="5" height="5" rx="1.2" fill="#818CF8" />
            <rect x="46" y="90" width="4.5" height="4.5" rx="1" fill="#38BDF8" />
          </g>
          {/* Whale at top-left */}
          <WhaleSprite x={18} y={16} scale={0.82} className="animate-mascot-float" />
          {/* Clawd at bottom-right */}
          <ClawdSprite x={96} y={64} scale={2.2} />
        </g>
      )}
    </svg>
  );
});

export default MascotIllustration;
