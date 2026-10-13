import express, { Router } from 'express';
import {
    addVideo,
    addVideoSchema,
    addVideoWithCaptions,
    deleteSource,
    getCourseMap,
    listSources,
    openExcerpt,
    organizeCourse,
    organizeSchema,
    uploadSource,
} from '@/controllers/sourceController.js';
import { requireAuth } from '@/middleware/authMiddleware.js';
import { rateLimit } from '@/middleware/rateLimitMiddleware.js';
import { validateBody } from '@/middleware/validationMiddleware.js';
import { asyncRoute } from '@/utils/http.js';

export const sourcesRouter = Router();

sourcesRouter.use(requireAuth);

// Ingesting spends model calls on figures, so uploads are limited. 30 an hour covers a whole
// course's worth of material in one sitting.
const ingestLimit = rateLimit({
    max: 30,
    windowMs: 60 * 60 * 1000,
    message: 'You have added a lot of material in the last hour.',
});

sourcesRouter.get('/', asyncRoute(listSources));

// The file is the body. 40 MB holds a full textbook; Claude's own PDF limit is 32 MB, and a larger
// book still has its text indexed, only its figures are skipped.
sourcesRouter.post(
    '/upload',
    ingestLimit,
    express.raw({ type: () => true, limit: '40mb' }),
    asyncRoute(uploadSource)
);

sourcesRouter.post('/video', ingestLimit, validateBody(addVideoSchema), asyncRoute(addVideo));

sourcesRouter.post(
    '/video/captions',
    ingestLimit,
    express.text({ type: () => true, limit: '5mb' }),
    asyncRoute(addVideoWithCaptions)
);

sourcesRouter.post(
    '/organize',
    rateLimit({
        max: 10,
        windowMs: 60 * 60 * 1000,
        message: 'You have rebuilt the course map a lot in the last hour.',
    }),
    validateBody(organizeSchema),
    asyncRoute(organizeCourse)
);

sourcesRouter.get('/map/:curriculumId', asyncRoute(getCourseMap));
sourcesRouter.get('/excerpts/:id/open', asyncRoute(openExcerpt));
sourcesRouter.delete('/:id', asyncRoute(deleteSource));
