export interface AppUpdateStatus {
  enabled: boolean;
  current_version: string;
  state: 'disabled' | 'idle' | 'checking' | 'available' | 'downloading' | 'ready' | 'installing' | 'error';
  available?: { version: string; notes: string; os: string; arch: string; url: string; sha256: string; size: number };
  last_checked?: string;
  error?: string;
  last_result?: { version: string; state: 'updated' | 'rolled_back' | 'failed'; error?: string; view_error?: string; executable_restored: boolean; data_restored: boolean };
}

export interface AppUpdateInstallRequest {
  version: string;
  sha256: string;
  open_mode?: 'web' | 'desktop' | 'cli';
}
