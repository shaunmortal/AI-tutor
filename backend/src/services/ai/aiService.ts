import type { AiRepository, MessageRow } from '@/repositories/aiRepository.js';
import type { IAIProvider } from '@/services/ai/IAIProvider.js';
import {
    GROUNDED_ANSWER_SCHEMA,
    GROUNDED_TUTOR_SYSTEM,
    TUTOR_SYSTEM,
    groundedTutorPrompt,
    tutorPrompt,
    type TutorContext,
} from '@/services/ai/prompts.js';
import type { Citation, SourceService } from '@/services/sources/sourceService.js';

export interface AskTutorInput {
    userId: string;
    question: string;
    /** Continues an existing thread; a new one is started when absent. */
    conversationId?: string;
    /** The topic in view, so the reply is grounded in what they know about it. */
    topicId?: string;
    /** The question on screen, when they asked from the practice page. */
    currentQuestion?: string;
}

export interface TutorReply {
    conversationId: string;
    title: string;
    answer: string;
    /**
     * 'source' / 'partial' / 'none' when the learner has course material and the answer was
     * grounded in it; null when they have none and the tutor answered from general knowledge.
     */
    grounding: Grounding | null;
    /** The excerpts the answer cites, numbered as they appear in it. */
    citations: Citation[];
    /** Both messages, so the client can render the exchange without a second request. */
    messages: { role: 'learner' | 'tutor'; content: string; createdAt: string; citations: Citation[] }[];
    /** So the cost of the feature is measurable rather than guessed at. */
    usage: { inputTokens: number; outputTokens: number; model: string };
}

/** How many earlier turns to replay. Enough to follow up; short enough to stay cheap. */
const HISTORY_TURNS = 8;

/** A learner's phone-typed question. Longer than this is an essay, not a doubt. */
const MAX_QUESTION = 2000;

type Grounding = 'source' | 'partial' | 'none';

/** Excerpts shown to the model per question. */
const EXCERPTS_PER_ANSWER = 6;

export class TutorService {
    private readonly ai: IAIProvider;
    private readonly conversations: AiRepository;
    /** Present when answers should come from the learner's own course material. */
    private readonly sources: SourceService | null;

    constructor(ai: IAIProvider, conversations: AiRepository, sources: SourceService | null = null) {
        this.ai = ai;
        this.conversations = conversations;
        this.sources = sources;
    }

    async listConversations(userId: string) {
        const rows = await this.conversations.listConversations(userId);
        const answered = await this.conversations.findAnsweredIds(rows.map((row) => row.id));

        return rows.map((row) => ({
            id: row.id,
            title: row.title,
            topicId: row.topic_id,
            lastMessageAt: row.last_message_at,
            // False when the question was asked but the model never answered — an outage, or a
            // request that failed after the question was saved. The list says so rather than
            // showing it as an ordinary thread.
            answered: answered.has(row.id),
        }));
    }

    async getConversation(conversationId: string, userId: string) {
        const conversation = await this.conversations.findConversation(conversationId, userId);
        const messages = await this.conversations.listMessages(conversation.id);

        return {
            id: conversation.id,
            title: conversation.title,
            topicId: conversation.topic_id,
            messages: messages.map(toView),
        };
    }

    /**
     * One question, one answer, both stored.
     *
     * Order matters. The learner's message is written before the model is called, so a question
     * asked during an outage is not lost — they can reload and see it, and ask again. The reply is
     * written after, which means a failed call leaves a conversation whose last message is
     * unanswered rather than a conversation that pretends nothing was asked.
     */
    async ask(input: AskTutorInput): Promise<TutorReply> {
        const question = input.question.trim().slice(0, MAX_QUESTION);

        const conversation = input.conversationId
            ? await this.conversations.findConversation(input.conversationId, input.userId)
            : await this.conversations.createConversation(
                  input.userId,
                  titleFrom(question),
                  input.topicId ?? null
              );

        // Earlier turns before the new question is stored, so the model is not shown the question
        // twice.
        const earlier = input.conversationId
            ? await this.conversations.listMessages(conversation.id)
            : [];

        await this.conversations.addMessage({
            conversationId: conversation.id,
            role: 'learner',
            content: question,
        });

        // The topic comes from the conversation when continuing one, so a follow-up stays grounded
        // in the same topic without the client having to resend it.
        const topicId = input.topicId ?? conversation.topic_id;
        const facts = await this.conversations.findTutorFacts(input.userId, topicId);

        const context: TutorContext = { ...facts, currentQuestion: input.currentQuestion ?? null };
        const history = earlier.slice(-HISTORY_TURNS).map((message) => ({
            role: message.role === 'learner' ? ('user' as const) : ('assistant' as const),
            content: message.content,
        }));

        const reply =
            this.sources && (await this.sources.hasMaterial(input.userId))
                ? await this.answerFromMaterial(input.userId, question, context, history)
                : await this.answerFreely(question, context, history);

        const answer = await this.conversations.addMessage({
            conversationId: conversation.id,
            role: 'tutor',
            content: reply.text,
            inputTokens: reply.usage.inputTokens,
            outputTokens: reply.usage.outputTokens,
            model: reply.model,
            citations: reply.grounding === null ? null : reply.citations,
        });

        return {
            conversationId: conversation.id,
            title: conversation.title,
            answer: reply.text,
            grounding: reply.grounding,
            citations: reply.citations,
            messages: [
                { role: 'learner', content: question, createdAt: answer.created_at, citations: [] },
                toView(answer),
            ],
            usage: { ...reply.usage, model: reply.model },
        };
    }

    private async answerFreely(
        question: string,
        context: TutorContext,
        history: { role: 'user' | 'assistant'; content: string }[]
    ) {
        const response = await this.ai.generateText({
            system: TUTOR_SYSTEM,
            prompt: tutorPrompt(question, context),
            // Sonnet-tier: a person reads this and judges it themselves, and the tutor is asked
            // dozens of questions in a session.
            quality: 'standard',
            maxTokens: 2000,
            history,
        });

        return {
            text: response.text,
            grounding: null,
            citations: [] as Citation[],
            usage: response.usage,
            model: response.model,
        };
    }

    private async answerFromMaterial(
        userId: string,
        question: string,
        context: TutorContext,
        history: { role: 'user' | 'assistant'; content: string }[]
    ) {
        return answerFromMaterial(this.ai, this.sources!, userId, question, context, history);
    }
}

/**
 * Answers from the learner's course material, with citations that open the exact page, slide
 * or second. Exported so the evaluation script runs exactly what the tutor runs.
 *
 * The model labels its own answer source, partial or none, but that label is not trusted on
 * its own: an answer only counts as source-backed if it cites an excerpt it was actually given.
 * Citation numbers that point at nothing are removed, and an "answer" that cites nothing is
 * moved to the general-knowledge section, where it is clearly marked as not from the course.
 */
export async function answerFromMaterial(
    ai: IAIProvider,
    sources: SourceService,
    userId: string,
    question: string,
    context: TutorContext,
    history: { role: 'user' | 'assistant'; content: string }[] = []
) {
    const query = [question, context.topicName].filter(Boolean).join(' ');
    const excerpts = await sources.retrieve(userId, query, EXCERPTS_PER_ANSWER);

    const response = await ai.generateJson({
        system: GROUNDED_TUTOR_SYSTEM,
        prompt: groundedTutorPrompt(
            question,
            context,
            excerpts.map((excerpt) => ({
                n: excerpt.n,
                label: `${excerpt.sourceTitle}, ${excerpt.location}`,
                text: excerpt.excerpt,
            }))
        ),
        schema: GROUNDED_ANSWER_SCHEMA,
        quality: 'standard',
        maxTokens: 2500,
        history,
    });

    return {
        ...checkGrounding(response.data, excerpts),
        /** Everything retrieved, cited or not: what RAGAS measures context precision against. */
        retrieved: excerpts,
        usage: response.usage,
        model: response.model,
    };
}

/** Pure, so the honesty rules above are unit-tested rather than hoped for. */
export function checkGrounding(
    raw: unknown,
    excerpts: Citation[]
): { text: string; grounding: Grounding; citations: Citation[] } {
    const reply = raw as { grounding?: string; answer?: string; outsideKnowledge?: string } | null;
    const known = new Map(excerpts.map((excerpt) => [excerpt.n, excerpt]));

    let answer = String(reply?.answer ?? '').trim();
    let outside = String(reply?.outsideKnowledge ?? '').trim();
    let grounding: Grounding =
        reply?.grounding === 'source' || reply?.grounding === 'partial' ? reply.grounding : 'none';

    // Drop citation markers that point at excerpts the model was never given.
    answer = answer.replace(/\[(\d+)\]/g, (marker, n: string) => (known.has(Number(n)) ? marker : ''));

    const cited = [...new Set([...answer.matchAll(/\[(\d+)\]/g)].map((match) => Number(match[1])))]
        .sort((a, b) => a - b)
        .map((n) => known.get(n)!);

    if (grounding !== 'none' && cited.length === 0) {
        outside = [answer, outside].filter(Boolean).join('\n\n');
        answer = '';
        grounding = 'none';
    }

    if (grounding === 'none') answer = '';

    const parts = [
        grounding === 'none' ? 'This is not covered in your course material.' : answer,
        outside && `General knowledge, not from your course material: ${outside}`,
    ].filter(Boolean);

    return { text: parts.join('\n\n'), grounding, citations: cited };
}

function toView(message: MessageRow) {
    return {
        role: message.role,
        content: message.content,
        createdAt: message.created_at,
        citations: (message.citations ?? []) as Citation[],
    };
}

/**
 * A readable title from the first question, so the history list means something at a glance.
 *
 * Cut at a word boundary rather than mid-word: a list of truncated questions is the only way a
 * learner finds the thread they want again.
 */
function titleFrom(question: string): string {
    const oneLine = question.replace(/\s+/g, ' ').trim();

    if (oneLine.length <= 60) return oneLine;

    const cut = oneLine.slice(0, 60);
    const lastSpace = cut.lastIndexOf(' ');

    return `${lastSpace > 20 ? cut.slice(0, lastSpace) : cut}…`;
}
