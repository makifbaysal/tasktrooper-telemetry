import { NextResponse } from 'next/server';
export async function POST(req: Request) {
  return NextResponse.json({ ok: true });
}
export const GET = async () => NextResponse.json({});
