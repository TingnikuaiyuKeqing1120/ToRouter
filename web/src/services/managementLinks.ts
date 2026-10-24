export function managementLink(page: 'model-specs' | 'prices', channelID: number, model: string) {
  return `#/${page}?${new URLSearchParams({ channel_id: String(channelID), model })}`;
}

export function managementTarget() {
  const query = new URLSearchParams(window.location.hash.split('?')[1] || '');
  const raw = query.get('channel_id');
  const channelID = raw == null || raw.trim() === '' ? null : Number(raw);
  return { channelID: channelID != null && Number.isInteger(channelID) && channelID >= 0 ? channelID : null, model: query.get('model') || '' };
}
