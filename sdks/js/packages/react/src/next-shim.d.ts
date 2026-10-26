// Minimal ambient types so this package builds without `next` installed (it is an optional peer dependency).
// Only the members used by ./nextjs.tsx are declared; they are not part of the published types.
declare module 'next/navigation' {
  export function usePathname(): string | null;
  export function useParams(): Record<string, string | string[]> | null;
}
declare module 'next/router' {
  export function useRouter(): { pathname: string; asPath: string; isReady: boolean };
}
