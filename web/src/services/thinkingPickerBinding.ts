export interface ThinkingRow { label: string; items: string[] }
export const defaultThinkingRows: ThinkingRow[] = [
  { label: '开关控制', items: ['disable', 'enable'] },
  { label: '常规档位', items: ['low', 'medium', 'high'] },
  { label: '极高档位', items: ['xhigh', 'max'] },
  { label: '极简档位', items: ['minimal', 'adaptive'] },
];
