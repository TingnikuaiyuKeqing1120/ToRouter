/**
 * API facade: re-exports domain types and services for backward-compatibility.
 * New code can directly import from '@/types' or '@/services/*'.
 */
export * from './types';
export * from './types/catalog-operations';
export * from './services/client';
export * from './services/formatters';
