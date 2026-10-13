import type { Request, Response } from 'express';
import { z } from 'zod';
import { adminDb } from '@/config/database.js';
import { authOf } from '@/middleware/authMiddleware.js';
import { CurriculumRepository } from '@/repositories/curriculumRepository.js';
import { SourceRepository } from '@/repositories/sourceRepository.js';
import { claudeProvider } from '@/services/ai/providers/claudeProvider.js';
import { SourceService } from '@/services/sources/sourceService.js';
import { youtubeId } from '@/services/sources/extractors.js';
import { AppError, sendOk } from '@/utils/http.js';

/** The service, wired with the elevated client. Every method takes the user id from the token. */
export function sourceService() {
    return new SourceService(
        claudeProvider(),
        new SourceRepository(adminDb),
        new CurriculumRepository(adminDb)
    );
}

const uploadQuerySchema = z.object({
    kind: z.enum(['pdf', 'slides']),
    title: z.string().trim().min(1).max(200),
});

/** POST /api/v1/sources/upload?kind=pdf&title=… — raw file bytes as the body. */
export async function uploadSource(req: Request, res: Response) {
    const { userId } = authOf(req);
    const query = uploadQuerySchema.safeParse(req.query);

    if (!query.success) {
        throw new AppError(400, 'INVALID_INPUT', 'Say what kind of file this is and give it a title.');
    }

    if (!Buffer.isBuffer(req.body) || req.body.length === 0) {
        throw new AppError(400, 'INVALID_INPUT', 'The upload was empty.');
    }

    const source = await sourceService().uploadFile({
        userId,
        kind: query.data.kind,
        title: query.data.title,
        file: new Uint8Array(req.body),
        fileName: query.data.title,
    });

    sendOk(res, { source: { id: source.id, status: source.status } }, 202);
}

export const addVideoSchema = z.object({
    url: z
        .string()
        .trim()
        .url()
        .refine((url) => youtubeId(url) !== null, 'Use a YouTube link to the lecture.'),
    title: z.string().trim().min(1).max(200),
});

/** POST /api/v1/sources/video — a YouTube lecture, transcribed from its published captions. */
export async function addVideo(req: Request, res: Response) {
    const { userId } = authOf(req);
    const body = req.body as z.infer<typeof addVideoSchema>;

    const source = await sourceService().addVideo({ userId, url: body.url, title: body.title });

    sendOk(res, { source: { id: source.id, status: source.status } }, 202);
}

/** POST /api/v1/sources/video/captions?url=…&title=… — a lecture with its .vtt/.srt as the body. */
export async function addVideoWithCaptions(req: Request, res: Response) {
    const { userId } = authOf(req);
    const query = addVideoSchema.safeParse(req.query);

    if (!query.success) {
        throw new AppError(400, 'INVALID_INPUT', query.error.issues[0]?.message ?? 'Invalid video.');
    }

    if (typeof req.body !== 'string' || !req.body.includes('-->')) {
        throw new AppError(400, 'INVALID_INPUT', 'That does not look like a .vtt or .srt file.');
    }

    const source = await sourceService().addVideo({
        userId,
        url: query.data.url,
        title: query.data.title,
        captions: req.body,
    });

    sendOk(res, { source: { id: source.id, status: source.status } }, 202);
}

/** GET /api/v1/sources */
export async function listSources(req: Request, res: Response) {
    const { userId } = authOf(req);

    sendOk(res, { sources: await sourceService().list(userId) });
}

/** DELETE /api/v1/sources/:id */
export async function deleteSource(req: Request, res: Response) {
    const { userId } = authOf(req);

    await sourceService().remove(uuidParam(req.params.id), userId);
    sendOk(res, { deleted: true });
}

export const organizeSchema = z.object({
    courseName: z.string().trim().min(2).max(120),
});

/** POST /api/v1/sources/organize — build the course map from everything uploaded so far. */
export async function organizeCourse(req: Request, res: Response) {
    const { userId } = authOf(req);
    const body = req.body as z.infer<typeof organizeSchema>;

    sendOk(res, await sourceService().organize(userId, body.courseName), 201);
}

/** GET /api/v1/sources/map/:curriculumId — topics, concepts, prerequisites, material per topic. */
export async function getCourseMap(req: Request, res: Response) {
    const { userId } = authOf(req);

    sendOk(res, await sourceService().courseMap(uuidParam(req.params.curriculumId), userId));
}

/** GET /api/v1/sources/excerpts/:id/open — where a citation chip should take the learner. */
export async function openExcerpt(req: Request, res: Response) {
    const { userId } = authOf(req);

    sendOk(res, await sourceService().openExcerpt(uuidParam(req.params.id), userId));
}

function uuidParam(value: unknown): string {
    const parsed = z.string().uuid().safeParse(value);

    if (!parsed.success) throw new AppError(400, 'INVALID_INPUT', 'That is not a valid id.');
    return parsed.data;
}
