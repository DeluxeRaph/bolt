use std::{
    collections::HashSet,
    fmt,
    future::Future,
    net::{SocketAddr, ToSocketAddrs},
    pin::Pin,
    str::FromStr,
    sync::Arc,
};

use alloy::primitives::{Address, Signature};
use axum::{extract::State, http::HeaderMap, routing::post, Json, Router};
use axum_extra::extract::WithRejection;
use serde_json::Value;
use tokio::{
    net::TcpListener,
    sync::{mpsc, oneshot},
};
use tracing::{debug, error, info, instrument};

use crate::{
    common::CARGO_PKG_VERSION,
    primitives::{
        commitment::{InclusionCommitment, SignedCommitment},
        CommitmentRequest, InclusionRequest,
    },
};

use super::{
    jsonrpc::{JsonPayload, JsonResponse},
    spec::{
        CommitmentsApi, Error, RejectionError, GET_VERSION_METHOD, REQUEST_INCLUSION_METHOD,
        SIGNATURE_HEADER,
    },
};

/// Event type emitted by the commitments API.
#[derive(Debug)]
pub struct Event {
    /// The request to process.
    pub request: CommitmentRequest,
    /// The response channel.
    pub response: oneshot::Sender<Result<SignedCommitment, Error>>,
}

/// The inner commitments-API handler that implements the [CommitmentsApi] spec.
/// Should be wrapped by a [CommitmentsApiServer] JSON-RPC server to handle requests.
#[derive(Debug)]
pub struct CommitmentsApiInner {
    /// Event notification channel
    events: mpsc::Sender<Event>,
    /// Optional whitelist of ECDSA public keys
    #[allow(unused)]
    whitelist: Option<HashSet<Address>>,
}

impl CommitmentsApiInner {
    /// Create a new API server with an optional whitelist of ECDSA public keys.
    pub fn new(events: mpsc::Sender<Event>) -> Self {
        Self { events, whitelist: None }
    }
}

#[async_trait::async_trait]
impl CommitmentsApi for CommitmentsApiInner {
    async fn request_inclusion(
        &self,
        inclusion_request: InclusionRequest,
    ) -> Result<InclusionCommitment, Error> {
        let (response_tx, response_rx) = oneshot::channel();

        let event = Event {
            request: CommitmentRequest::Inclusion(inclusion_request),
            response: response_tx,
        };

        self.events.send(event).await.unwrap();

        response_rx.await.map_err(|_| Error::Internal)?.map(|c| c.into())
    }
}

/// The outer commitments-API JSON-RPC server that wraps the [CommitmentsApiInner] handler.
pub struct CommitmentsApiServer {
    /// The address to bind the server to. This will be updated
    /// with the actual address after the server is started.
    addr: SocketAddr,
    /// The shutdown signal.
    signal: Option<Pin<Box<dyn Future<Output = ()> + Send>>>,
}

impl fmt::Debug for CommitmentsApiServer {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("CommitmentsApiServer").field("addr", &self.addr).finish()
    }
}

impl CommitmentsApiServer {
    /// Creates the server with the given address and default shutdown signal (CTRL+C).
    pub fn new<A: ToSocketAddrs>(addr: A) -> Self {
        Self {
            addr: addr.to_socket_addrs().unwrap().next().unwrap(),
            signal: Some(Box::pin(async {
                let _ = tokio::signal::ctrl_c().await;
            })),
        }
    }

    /// Creates the server with the given address and shutdown signal.
    pub fn with_shutdown<A, S>(self, addr: A, signal: S) -> Self
    where
        A: ToSocketAddrs,
        S: Future<Output = ()> + Send + 'static,
    {
        Self {
            addr: addr.to_socket_addrs().unwrap().next().unwrap(),
            signal: Some(Box::pin(signal)),
        }
    }

    /// Runs the JSON-RPC server, sending events to the provided channel.
    pub async fn run(&mut self, events_tx: mpsc::Sender<Event>) {
        let api = Arc::new(CommitmentsApiInner::new(events_tx));

        let router = Router::new().route("/", post(Self::handle_rpc)).with_state(api);

        let listener = match TcpListener::bind(self.addr).await {
            Ok(listener) => listener,
            Err(err) => {
                error!(?err, "Failed to bind Commitments API server");
                panic!("Failed to bind Commitments API server");
            }
        };

        let addr = listener.local_addr().expect("Failed to get local address");
        self.addr = addr;

        info!("Commitments RPC server bound to {addr}");

        let signal = self.signal.take().expect("Signal not set");

        tokio::spawn(async move {
            if let Err(err) = axum::serve(listener, router).with_graceful_shutdown(signal).await {
                error!(?err, "Commitments API Server error");
            }
        });
    }

    /// Returns the local addr the server is listening on (or configured with).
    pub fn local_addr(&self) -> SocketAddr {
        self.addr
    }

    /// Handler function for the root JSON-RPC path.
    #[instrument(skip_all, name = "RPC", fields(method = %payload.method))]
    async fn handle_rpc(
        headers: HeaderMap,
        State(api): State<Arc<CommitmentsApiInner>>,
        WithRejection(Json(payload), _): WithRejection<Json<JsonPayload>, Error>,
    ) -> Result<Json<JsonResponse>, Error> {
        debug!("Received new request");

        let (signer, signature) = auth_from_headers(&headers).inspect_err(|e| {
            error!("Failed to extract signature from headers: {:?}", e);
        })?;

        match payload.method.as_str() {
            GET_VERSION_METHOD => {
                let version_string = format!("bolt-sidecar-v{CARGO_PKG_VERSION}");
                Ok(Json(JsonResponse {
                    id: payload.id,
                    result: Value::String(version_string),
                    ..Default::default()
                }))
            }

            REQUEST_INCLUSION_METHOD => {
                let Some(request_json) = payload.params.first().cloned() else {
                    return Err(RejectionError::ValidationFailed("Bad params".to_string()).into());
                };

                // Parse the inclusion request from the parameters
                let mut inclusion_request: InclusionRequest = serde_json::from_value(request_json)
                    .map_err(|e| RejectionError::ValidationFailed(e.to_string()))?;

                // Set the signature here for later processing
                inclusion_request.set_signature(signature);

                let digest = inclusion_request.digest();
                let recovered_signer = signature.recover_address_from_prehash(&digest)?;

                if recovered_signer != signer {
                    error!(
                        ?recovered_signer,
                        ?signer,
                        "Recovered signer does not match the provided signer"
                    );

                    return Err(Error::InvalidSignature(crate::primitives::SignatureError));
                }

                // Set the request signer
                inclusion_request.set_signer(recovered_signer);

                info!(signer = ?recovered_signer, %digest, "New valid inclusion request received");
                let inclusion_commitment = api.request_inclusion(inclusion_request).await?;

                // Create the JSON-RPC response
                let response = JsonResponse {
                    id: payload.id,
                    result: serde_json::to_value(inclusion_commitment).unwrap(),
                    ..Default::default()
                };

                Ok(Json(response))
            }
            other => {
                error!("Unknown method: {}", other);
                Err(Error::UnknownMethod)
            }
        }
    }
}

/// Extracts the signature ([SIGNATURE_HEADER]) from the HTTP headers.
#[inline]
fn auth_from_headers(headers: &HeaderMap) -> Result<(Address, Signature), Error> {
    let auth = headers.get(SIGNATURE_HEADER).ok_or(Error::NoSignature)?;

    // Remove the "0x" prefix
    let auth = auth.to_str().map_err(|_| Error::MalformedHeader)?;

    let mut split = auth.split(':');

    let address = split.next().ok_or(Error::MalformedHeader)?;
    let address = Address::from_str(address).map_err(|_| Error::MalformedHeader)?;

    let sig = split.next().ok_or(Error::MalformedHeader)?;
    let sig = Signature::from_str(sig)
        .map_err(|_| Error::InvalidSignature(crate::primitives::SignatureError))?;

    Ok((address, sig))
}

#[cfg(test)]
mod test {
    use alloy::{
        primitives::TxHash,
        signers::{k256::SecretKey, local::PrivateKeySigner, Signer},
    };
    use serde_json::json;

    use crate::{
        primitives::commitment::ECDSASignatureExt,
        test_util::{create_signed_commitment_request, default_test_transaction},
    };

    use super::*;

    #[tokio::test]
    async fn test_signature_from_headers() {
        let mut headers = HeaderMap::new();
        let hash = TxHash::random();
        let signer = PrivateKeySigner::random();
        let addr = signer.address();

        let expected_sig = signer.sign_hash(&hash).await.unwrap();
        headers
            .insert(SIGNATURE_HEADER, format!("{addr}:{}", expected_sig.to_hex()).parse().unwrap());

        let (address, signature) = auth_from_headers(&headers).unwrap();
        assert_eq!(signature, expected_sig);
        assert_eq!(address, addr);
    }

    #[tokio::test]
    async fn test_request_unauthorized() {
        let _ = tracing_subscriber::fmt::try_init();

        let mut server = CommitmentsApiServer::new("0.0.0.0:0");

        let (events_tx, _) = mpsc::channel(1);

        server.run(events_tx).await;
        let addr = server.local_addr();

        let sk = SecretKey::random(&mut rand::thread_rng());
        let signer = PrivateKeySigner::from(sk.clone());
        let tx = default_test_transaction(signer.address(), None);
        let req = create_signed_commitment_request(&[tx], &sk, 12).await.unwrap();

        let payload = json!({
            "jsonrpc": "2.0",
            "id": 1,
            "method": "bolt_requestInclusion",
            "params": [req]
        });

        let url = format!("http://{addr}");

        let client = reqwest::Client::new();
        // client.post(url).header("content-type", "application/json").body(payload)

        let response = client
            .post(url)
            .json(&payload)
            .send()
            .await
            .unwrap()
            .json::<JsonResponse>()
            .await
            .unwrap();

        // Assert unauthorized because of missing signature
        assert_eq!(response.error.unwrap().code, -32003);
    }

    #[tokio::test]
    async fn test_request_success() {
        let _ = tracing_subscriber::fmt::try_init();

        let mut server = CommitmentsApiServer::new("0.0.0.0:0");

        let (events_tx, mut events) = mpsc::channel(1);

        server.run(events_tx).await;
        let addr = server.local_addr();

        let sk = SecretKey::random(&mut rand::thread_rng());
        let signer = PrivateKeySigner::from(sk.clone());
        let tx = default_test_transaction(signer.address(), None);
        let req = create_signed_commitment_request(&[tx], &sk, 12).await.unwrap();

        let sig = req.signature().unwrap().to_hex();

        let payload = json!({
            "jsonrpc": "2.0",
            "id": 1,
            "method": "bolt_requestInclusion",
            "params": [req]
        });

        let url = format!("http://{addr}");

        let client = reqwest::Client::new();
        // client.post(url).header("content-type", "application/json").body(payload)

        let (tx, rx) = oneshot::channel();

        tokio::spawn(async move {
            let response = client
                .post(url)
                .header(SIGNATURE_HEADER, format!("{}:{}", signer.address(), sig))
                .json(&payload)
                .send()
                .await
                .unwrap();

            let json = response.json::<JsonResponse>().await.unwrap();

            // Assert unauthorized because of missing signature
            assert!(json.error.is_none());

            let _ = tx.send(());
        });

        let Event { request, response } = events.recv().await.unwrap();

        let commitment_signer = PrivateKeySigner::random();

        let commitment = request.commit_and_sign(&commitment_signer).await.unwrap();

        response.send(Ok(commitment)).unwrap();

        rx.await.unwrap();
    }
}
