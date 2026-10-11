// Mock switch. The public variable injected at build time (only the NEXT_PUBLIC_ prefix is readable in the browser).
// On Vercel, set NEXT_PUBLIC_MOCK=1 to run the whole site against mock data with no backend.
export const MOCK = process.env.NEXT_PUBLIC_MOCK === "1";
