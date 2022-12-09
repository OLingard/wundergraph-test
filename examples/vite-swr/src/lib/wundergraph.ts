import { createClient, Operations } from '../../components/generated/client';

import { createHooks } from '@wundergraph/swr';

export const client = createClient();

export const { useQuery, useMutation, useSubscription, useUser, useAuth } = createHooks<Operations>(client);
